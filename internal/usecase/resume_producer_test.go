package usecase

import (
	"context"
	"crypto/sha1"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"torrent-stream-hub/internal/fastresume"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
)

// Protocol fixture only: native flush/evidence and real library hashes are
// exercised separately by repository/fork suites, not synthesized by this test.
func resumeProducerFixture(t *testing.T, capture func(context.Context, func(storage.FileCheckpoint) error, storage.FileCheckpoint) error) (*TorrentUseCase, *repository.StorageMapping, repository.ResumeSource, *applicationResumeProducer) {
	t.Helper()
	uc, repo, cleanup := setupTorrentUseCase(t, true)
	t.Cleanup(cleanup)
	pieceHash := sha1.Sum([]byte("data"))
	info := &metainfo.Info{Name: "film", Length: 4, PieceLength: 4, Pieces: pieceHash[:]}
	raw, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: raw}
	hash := mi.HashInfoBytes().HexString()
	if err := repo.SaveTorrent(&models.Torrent{Hash: hash, Name: "film", State: models.StatePaused}); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureStorageMapping(&repository.StorageMapping{Hash: hash, Version: 2, Root: filepath.Join("payload", "film"), State: "ready"}); err != nil {
		t.Fatal(err)
	}
	m, err := repo.StorageMapping(hash)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := repo.GetIntent(hash)
	if err != nil {
		t.Fatal(err)
	}
	native := storage.FileCheckpoint{InfoHash: mi.HashInfoBytes(), Root: m.Root, PieceLength: 4, PieceCount: 1, ExtentLength: 4, Complete: []byte{1}, Files: []storage.CheckpointFile{{Path: "film", Length: 4, Present: true, Identity: "fixture", Size: 4, ModifiedNS: 1}}}
	source := repository.ResumeSource{Identity: fastresume.Identity{RecordID: intent.Version.RecordID, StorageID: m.StorageID, MappingRevision: m.Revision, LayoutVersion: m.Version, Root: m.Root, VolumeID: "fixture-volume"}, Metadata: mi, Info: info, Authorize: func(ctx context.Context) error { return ctx.Err() }, Capture: func(ctx context.Context, publish func(storage.FileCheckpoint) error) error {
		return capture(ctx, publish, native)
	}}
	p, err := newApplicationResumeProducer(uc, func(context.Context, string) (string, *repository.StorageMapping, repository.ResumeSource, error) {
		return hash, m, source, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()); _ = p.Wait() })
	return uc, m, source, p
}

func TestApplicationResumeProducerShutdownUsesDrainOwnership(t *testing.T) {
	var uc *TorrentUseCase
	var captured bool
	fixture, m, source, p := resumeProducerFixture(t, func(ctx context.Context, publish func(storage.FileCheckpoint) error, native storage.FileCheckpoint) error {
		if ctx.Err() != nil || uc.coordinator.ctx.Err() == nil {
			t.Error("drain confused runtime and shutdown contexts")
		}
		uc.coordinator.mu.Lock()
		reserved := uc.coordinator.refs
		uc.coordinator.mu.Unlock()
		if reserved == 0 {
			t.Error("seal omitted existing command ownership")
		}
		captured = true
		return publish(native)
	})
	uc = fixture
	if !uc.engine.RegisterStorageFinalizer(p) || !p.MarkDirty(m.StorageID) {
		t.Fatal("producer admission failed")
	}
	uc.engine.Close()
	if err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	if !captured {
		t.Fatal("shutdown omitted pending checkpoint")
	}
	saved, err := uc.repo.LoadResume(context.Background(), m, source.Identity.RecordID)
	if err != nil || saved.Invalidated || saved.Token.Generation != 1 {
		t.Fatal("drain failed to commit", saved, err)
	}
	if _, err := uc.lockRecord(m.Hash, context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatal("drain revived normal command admission", err)
	}
	if p.MarkDirty(m.StorageID) {
		t.Fatal("closed producer accepted work")
	}
}

func TestApplicationResumeProducerCancelledSealRetainsOwnership(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	uc, m, source, p := resumeProducerFixture(t, func(ctx context.Context, publish func(storage.FileCheckpoint) error, native storage.FileCheckpoint) error {
		close(entered)
		<-release
		return publish(native)
	})
	t.Cleanup(unblock)
	p.MarkDirty(m.StorageID)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	go func() { returned <- p.Shutdown(ctx) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not enter capture")
	}
	cancel()
	if err := <-returned; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	uc.coordinator.mu.Lock()
	refs := uc.coordinator.refs
	uc.coordinator.mu.Unlock()
	if refs == 0 {
		t.Fatal("deadline released unjoined capture ownership")
	}
	unblock()
	if err := p.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled seal succeeded", err)
	}
	saved, err := uc.repo.LoadResume(context.Background(), m, source.Identity.RecordID)
	if err != nil || !saved.Invalidated || saved.Token.Generation != 0 {
		t.Fatal("cancelled publication created checkpoint", saved, err)
	}
}

func TestApplicationResumeProducerRevalidatesLookupAfterReservation(t *testing.T) {
	for _, failure := range []string{"owner", "mapping", "record", "lookup"} {
		t.Run(failure, func(t *testing.T) {
			uc, m, source, p := resumeProducerFixture(t, func(context.Context, func(storage.FileCheckpoint) error, storage.FileCheckpoint) error {
				t.Error("invalid lookup reached capture")
				return nil
			})
			calls := 0
			p.lookup = func(context.Context, string) (string, *repository.StorageMapping, repository.ResumeSource, error) {
				calls++
				current, bound := *m, source
				if failure == "owner" {
					current.StorageID = "other"
				}
				if calls > 1 {
					if failure == "mapping" {
						current.Revision++
					}
					if failure == "record" {
						bound.Identity.RecordID = "new-record"
					}
					if failure == "lookup" {
						return "", nil, bound, ErrStorageConflict
					}
				}
				return m.Hash, &current, bound, nil
			}
			_, _, release, err := p.acquireSource(context.Background(), m.StorageID)
			if release != nil {
				release()
			}
			if !errors.Is(err, ErrStorageConflict) {
				t.Fatal("stale lookup acquired source", err)
			}
			uc.coordinator.mu.Lock()
			refs := uc.coordinator.refs
			uc.coordinator.mu.Unlock()
			if refs != 0 {
				t.Fatal("failed lookup retained reservations", refs)
			}
		})
	}
}
