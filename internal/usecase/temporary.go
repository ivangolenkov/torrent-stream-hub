package usecase

import (
	"context"
	"fmt"
	"sync"
	"time"

	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"
)

// Record/root command handles serialize lease admission with policy/deletion.
// The release carries record identity, not a hash-only counter capability.
func (uc *TorrentUseCase) AcquireTransfer(hash string, contexts ...context.Context) (func(), error) {
	release, err := uc.lockRecord(hash, contexts...)
	if err != nil {
		return nil, err
	}
	defer release()
	t, err := uc.repo.GetTorrent(hash)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, TorrentNotFoundError{Hash: hash}
	}
	if err := uc.checkStorageAvailable(hash); err != nil {
		return nil, err
	}
	if _, pending, err := uc.repo.CleanupPaths(hash); err != nil {
		return nil, err
	} else if pending {
		return nil, ErrStorageConflict
	}
	if t.Control == nil {
		return nil, ErrStorageConflict
	}
	record := t.Control.RecordID
	uc.policyMu.Lock()
	if uc.transferRecords[hash] != record {
		uc.transfers[hash] = 0
		delete(uc.protectedUntil, hash)
		uc.transferRecords[hash] = record
	}
	uc.transfers[hash]++
	uc.policyMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			uc.policyMu.Lock()
			defer uc.policyMu.Unlock()
			if uc.transferRecords[hash] != record {
				return
			}
			uc.transfers[hash]--
			if uc.transfers[hash] == 0 {
				delete(uc.transfers, hash)
				uc.protectedUntil[hash] = time.Now().Add(engine.DebounceDelay)
			}
		})
	}, nil
}

func (uc *TorrentUseCase) MakePermanent(hash string, contexts ...context.Context) error {
	release, err := uc.lockRecord(hash, contexts...)
	if err != nil {
		return err
	}
	defer release()
	expected := uc.engine.GetRawTorrent(hash)
	i, err := uc.currentIntent(hash)
	if err != nil {
		return err
	}
	found, err := uc.repo.MakePermanent(hash)
	if err != nil {
		return err
	}
	if !found {
		return TorrentNotFoundError{Hash: hash}
	}
	i.Version.Revision++
	if err := bindIncrementalRuntime(uc.engine, uc.repo, i.Version); err != nil {
		return uc.failedIntentApply(expected, i.Version, "runtime_bind_failed", err)
	}
	logging.Infof("torrent made permanent hash=%s", hash)
	return nil
}

func (uc *TorrentUseCase) finishAddLocked(t *models.Torrent, metadata TorrentMetadata, temporary bool) (*models.Torrent, error) {
	existing, err := uc.repo.GetTorrent(t.Hash)
	if err != nil {
		return nil, err
	}
	mergePersistedMetadata(t, existing)
	applyMetadata(t, metadata)
	if existing == nil {
		n := models.NewTorrentRecord{Hash: t.Hash, Name: t.Name, SourceURI: t.SourceURI, Run: models.RunEnabled, Temporary: temporary, Metadata: models.UserMetadata{Title: t.Title, Data: t.Data, Poster: t.Poster, Category: t.Category}}
		if t.MetadataKnown {
			c := runtimeCatalog(t)
			n.Catalog = &c
		}
		if err := uc.repo.CreateTorrent(n); err != nil {
			_ = uc.engine.Delete(t.Hash)
			return nil, err
		}
	} else {
		i, err := uc.currentIntent(t.Hash)
		if err != nil {
			return nil, err
		}
		if metadata != (TorrentMetadata{}) {
			if _, err := uc.repo.UpdateUserMetadata(i.Version, models.UserMetadata{Title: metadata.Title, Data: metadata.Data, Poster: metadata.Poster, Category: metadata.Category}); err != nil {
				return nil, err
			}
		}
		if !temporary {
			if _, err := uc.repo.MakePermanent(t.Hash); err != nil {
				return nil, err
			}
		}
	}
	saved, err := uc.repo.GetTorrent(t.Hash)
	if err != nil {
		return nil, err
	}
	uc.engine.SetQueuePosition(t.Hash, saved.QueuePosition)
	i, err := uc.currentIntent(t.Hash)
	if err != nil {
		return nil, err
	}
	expected := uc.engine.GetRawTorrent(t.Hash)
	if existing == nil {
		if err := uc.engine.FinishRestore(t.Hash, models.StateQueued); err != nil {
			return nil, uc.failedIntentApply(expected, i.Version, "runtime_apply_failed", err)
		}
	}
	bind := bindAppliedRuntime
	if existing != nil {
		bind = bindIncrementalRuntime
	}
	if err := bind(uc.engine, uc.repo, i.Version); err != nil {
		return nil, uc.failedIntentApply(expected, i.Version, "runtime_bind_failed", err)
	}
	t.IsTemporary = temporary && (existing == nil || existing.IsTemporary)
	result, err := uc.GetTorrent(t.Hash)
	if err != nil {
		return nil, err
	}
	if temporary && existing == nil {
		uc.cleanupLocked("add")
	}
	return result, nil
}

func (uc *TorrentUseCase) SetTemporaryLimit(limit int) error {
	if limit < 1 {
		return fmt.Errorf("max temporary torrents must be positive")
	}
	uc.policyMu.Lock()
	defer uc.policyMu.Unlock()
	uc.maxTemporary = limit
	return nil
}

func (uc *TorrentUseCase) CleanupTemporary(reason string) {
	uc.cleanupLocked(reason)
}

func (uc *TorrentUseCase) RunTemporaryCleanup(ctx context.Context) {
	uc.runTemporaryCleanup(ctx, 5*time.Second)
}

func (uc *TorrentUseCase) runTemporaryCleanup(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			uc.CleanupTemporary("retry")
		}
	}
}

func (uc *TorrentUseCase) cleanupLocked(reason string) {
	if !uc.cleanupMu.TryLock() {
		return
	}
	defer uc.cleanupMu.Unlock()
	uc.policyMu.Lock()
	for hash, until := range uc.protectedUntil {
		if time.Now().After(until) {
			delete(uc.protectedUntil, hash)
			if uc.transfers[hash] == 0 {
				delete(uc.transferRecords, hash)
			}
		}
	}
	limit := uc.maxTemporary
	uc.policyMu.Unlock()
	queue, err := uc.repo.TemporaryQueue()
	if err != nil {
		logging.Warnf("temporary cleanup reason=%s: %v", reason, err)
		return
	}
	for len(queue) > limit {
		hash := queue[0]
		release, err := uc.tryLockRecord(hash)
		if err != nil {
			return
		} // Retry the FIFO head later, never wait while Add owns another key.
		current, err := uc.repo.TemporaryQueue()
		if err != nil || len(current) == 0 || current[0] != hash {
			release()
			return
		}
		queue = current
		uc.policyMu.Lock()
		limit = uc.maxTemporary
		uc.policyMu.Unlock()
		if len(queue) <= limit {
			release()
			return
		}
		if uc.transferProtected(hash) {
			release()
			logging.Debugf("temporary cleanup deferred reason=%s hash=%s active transfer or debounce", reason, hash)
			return
		}
		if err := uc.deleteTemporaryLocked(hash); err != nil {
			release()
			logging.Warnf("temporary cleanup failed reason=%s hash=%s: %v", reason, hash, err)
			return
		}
		uc.policyMu.Lock()
		delete(uc.protectedUntil, hash)
		if uc.transfers[hash] == 0 {
			delete(uc.transferRecords, hash)
		}
		uc.policyMu.Unlock()
		release()
		logging.Infof("temporary cleanup removed reason=%s hash=%s", reason, hash)
		queue = queue[1:]
	}
}

func (uc *TorrentUseCase) transferProtected(hash string) bool {
	uc.policyMu.Lock()
	protected := uc.transfers[hash] > 0 || time.Now().Before(uc.protectedUntil[hash])
	uc.policyMu.Unlock()
	return protected || uc.engine.StreamManager().ActiveStreamsForTorrent(hash) > 0
}

func (uc *TorrentUseCase) transferCount(hash string) int {
	uc.policyMu.Lock()
	defer uc.policyMu.Unlock()
	return uc.transfers[hash]
}
