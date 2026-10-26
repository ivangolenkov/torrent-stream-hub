package usecase

import (
	"context"
	"errors"
	"sync"

	"torrent-stream-hub/internal/fastresume"
	"torrent-stream-hub/internal/repository"
)

// resumeOwnerSource is a bounded, cached owner lookup, not a library-wide scan.
// It is called again under the existing command reservations before capture.
// Authorize must bind the canonical runtime pointer, record, mapping and live
// volume/inventory/safety gates; it may not reacquire command keys.
type resumeOwnerSource func(context.Context, string) (string, *repository.StorageMapping, repository.ResumeSource, error)

type applicationResumeProducer struct {
	uc          *TorrentUseCase
	lookup      resumeOwnerSource
	worker      *fastresume.SealWorker
	mu          sync.Mutex
	stopping    bool
	ready       chan struct{}
	drain       *coordinatorDrain
	drainErr    error
	shutdownErr error
	done        chan struct{}
	once        sync.Once
	finalScan   func(context.Context, func(string) bool) error
}

func newApplicationResumeProducer(uc *TorrentUseCase, lookup resumeOwnerSource) (*applicationResumeProducer, error) {
	if lookup == nil {
		return nil, errors.New("nil application resume source")
	}
	p := &applicationResumeProducer{uc: uc, lookup: lookup, ready: make(chan struct{}), done: make(chan struct{})}
	w, err := uc.repo.NewResumeProducer(p.acquireSource)
	if err != nil {
		return nil, err
	}
	p.worker = w
	return p, nil
}

func (p *applicationResumeProducer) acquireSource(ctx context.Context, owner string) (*repository.StorageMapping, repository.ResumeSource, func(), error) {
	p.mu.Lock()
	stopping := p.stopping
	p.mu.Unlock()
	var drain *coordinatorDrain
	if stopping {
		select {
		case <-ctx.Done():
			return nil, repository.ResumeSource{}, nil, ctx.Err()
		case <-p.ready:
		}
		p.mu.Lock()
		drain, err := p.drain, p.drainErr
		p.mu.Unlock()
		if err != nil {
			return nil, repository.ResumeSource{}, nil, err
		}
		return p.acquireOwnedSource(ctx, owner, drain)
	}
	return p.acquireOwnedSource(ctx, owner, drain)
}

func (p *applicationResumeProducer) acquireOwnedSource(ctx context.Context, owner string, drain *coordinatorDrain) (*repository.StorageMapping, repository.ResumeSource, func(), error) {
	hash, m, source, err := p.lookup(ctx, owner)
	if err != nil {
		return nil, source, nil, err
	}
	if hash == "" || m == nil || m.StorageID != owner {
		return nil, source, nil, ErrStorageConflict
	}
	expected := *m
	var release func()
	if drain == nil {
		release, err = p.uc.lockRecord(hash, ctx)
	} else {
		release, err = p.uc.lockRecordsForDrain(ctx, drain, hash)
	}
	if err != nil {
		return nil, source, nil, err
	}
	currentHash, current, bound, err := p.lookup(ctx, owner)
	if err != nil || currentHash != hash || current == nil || *current != expected || bound.Identity.RecordID != source.Identity.RecordID {
		release()
		if err == nil {
			err = ErrStorageConflict
		}
		return nil, bound, nil, err
	}
	return current, bound, release, nil
}

func (p *applicationResumeProducer) MarkDirty(owner string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.stopping && p.worker.MarkDirty(owner)
}

// Shutdown first closes normal coordinator admission and joins its references,
// then uses a unique shutdown-only capability on that SAME key registry. Worker
// stop is requested concurrently so an active seal receives the shutdown budget
// while coordinator cleanup is waiting for its reservation. New drain callbacks
// wait on ready BEFORE taking any references; they cannot deadlock beginDrain.
func (p *applicationResumeProducer) Shutdown(ctx context.Context) error {
	p.once.Do(func() {
		p.mu.Lock()
		p.stopping = true
		p.mu.Unlock()
		go func() {
			var scanErr error
			if p.finalScan != nil {
				scanErr = p.finalScan(ctx, p.worker.MarkDirty)
			}
			err := errors.Join(scanErr, p.worker.Shutdown(ctx))
			p.mu.Lock()
			p.shutdownErr = err
			p.mu.Unlock()
			close(p.done)
		}()
		go func() {
			drain, err := p.uc.coordinator.beginDrain(ctx)
			p.mu.Lock()
			p.drain, p.drainErr = drain, err
			p.mu.Unlock()
			close(p.ready)
		}()
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.shutdownErr
	}
}

func (p *applicationResumeProducer) Wait() error {
	err := p.worker.Wait()
	<-p.done
	<-p.ready // join the issued ownership transition, even after deadline
	p.mu.Lock()
	drain, drainErr := p.drain, p.drainErr
	p.mu.Unlock()
	if drain != nil {
		drainErr = errors.Join(drainErr, drain.finish())
	}
	return errors.Join(err, drainErr)
}
