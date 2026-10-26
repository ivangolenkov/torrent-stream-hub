package fastresume

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// SealFunc must retain application mutation/attachment ownership until capture,
// flush and durable publication have joined. Cancellation is not permission to
// abandon a syscall or release that ownership. It runs on the single worker,
// never under the queue mutex. It must not call Shutdown or Wait on this worker.
type SealFunc func(context.Context, string) error

// SealWorker is a bounded scheduling primitive, not persistent authority. The
// producer retains its dirty state until a successful seal; a rejected enqueue
// must be retried during the next bounded inventory scan. In particular, durable
// revocations MUST NOT be sent through this lossy optimization queue.
// The application creates one worker, reports errors in SealFunc, stops its
// producers before Shutdown, and joins Wait before closing shared resources.
type SealWorker struct {
	mu         sync.Mutex
	owners     map[string]*sealRequest // includes the currently executing owner
	queue      []string
	closing    bool
	closeCtx   context.Context
	err        error
	stopping   chan struct{}
	done       chan struct{}
	seal       SealFunc
	ticks      <-chan time.Time
	stopTick   func()
	jobCtx     context.Context
	cancel     context.CancelFunc
	stopCancel func() bool
}

type sealRequest struct{ dirty bool }

func NewSealWorker(seal SealFunc) (*SealWorker, error) {
	if seal == nil {
		return nil, errors.New("nil checkpoint seal callback")
	}
	ticker := time.NewTicker(CheckpointInterval)
	return newSealWorker(seal, ticker.C, ticker.Stop), nil
}

func newSealWorker(seal SealFunc, ticks <-chan time.Time, stop func()) *SealWorker {
	ctx, cancel := context.WithCancel(context.Background())
	w := &SealWorker{owners: make(map[string]*sealRequest), stopping: make(chan struct{}), done: make(chan struct{}), seal: seal, ticks: ticks, stopTick: stop, jobCtx: ctx, cancel: cancel}
	go w.run()
	return w
}

// MarkDirty coalesces pending requests, including changes during a seal. False
// means invalid owner, saturation or stopped admission, not a successful seal.
func (w *SealWorker) MarkDirty(owner string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closing || owner == "" || len(owner) > MaxIdentityLength {
		return false
	}
	if request := w.owners[owner]; request != nil {
		request.dirty = true
		return true
	}
	if len(w.owners) >= MaxPendingOwners {
		return false
	}
	w.owners[owner] = &sealRequest{dirty: true}
	w.queue = append(w.queue, owner)
	return true
}

// Shutdown stops admission and drains each queued owner at most once. The first
// caller supplies the independent shutdown budget (not the stopped runtime ctx).
// On deadline it can return BEFORE a blocked syscall joins: the application must
// still Wait before closing SQLite, storage or instance ownership. Later callers
// may use another waiting budget, but do not replace the original drain context.
func (w *SealWorker) Shutdown(ctx context.Context) error {
	w.mu.Lock()
	if !w.closing {
		w.closing, w.closeCtx = true, ctx
		w.stopCancel = context.AfterFunc(ctx, w.cancel)
		close(w.stopping)
	}
	w.mu.Unlock()
	select {
	case <-w.done:
		return w.Wait()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Done closes only after all issued callbacks have returned, even if Shutdown
// timed out. Wait is the resource-lifetime join, not merely cancellation.
func (w *SealWorker) Done() <-chan struct{} { return w.done }
func (w *SealWorker) Wait() error {
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *SealWorker) run() {
	defer func() {
		w.stopTick()
		w.cancel()
		w.mu.Lock()
		if w.stopCancel != nil {
			w.stopCancel()
		}
		w.mu.Unlock()
		close(w.done)
	}()
	for {
		w.mu.Lock()
		closing, ctx := w.closing, w.closeCtx
		w.mu.Unlock()
		if closing {
			err := w.batch(ctx, true)
			w.mu.Lock()
			w.err = err
			w.mu.Unlock()
			return
		}
		select {
		case <-w.stopping:
		case <-w.ticks:
			w.batch(w.jobCtx, false)
		}
	}
}

func (w *SealWorker) batch(ctx context.Context, drain bool) error {
	w.mu.Lock()
	count := len(w.queue)
	w.mu.Unlock()
	var failures []error
	for i := 0; i < count; i++ {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		w.mu.Lock()
		if !drain && w.closing {
			w.mu.Unlock()
			break
		}
		owner := w.queue[0]
		w.queue = w.queue[1:]
		request := w.owners[owner]
		request.dirty = false
		w.mu.Unlock()

		err := w.seal(ctx, owner)
		w.mu.Lock()
		if err != nil || request.dirty {
			w.queue = append(w.queue, owner)
		} else {
			delete(w.owners, owner)
		}
		w.mu.Unlock()
		if err != nil {
			failures = append(failures, fmt.Errorf("checkpoint %s: %w", owner, err))
		}
	}
	return errors.Join(append(failures, ctx.Err())...)
}
