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

// All policy changes, transfer admission and cleanup share repo.Lifecycle.
// Leases outlive the lock, so slow players never hold it during network I/O.
func (uc *TorrentUseCase) AcquireTransfer(hash string) (func(), error) {
	uc.repo.Lifecycle.Lock()
	defer uc.repo.Lifecycle.Unlock()
	t, err := uc.repo.GetTorrent(hash)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, TorrentNotFoundError{Hash: hash}
	}
	if _, pending, err := uc.repo.CleanupPaths(hash); err != nil {
		return nil, err
	} else if pending {
		return nil, ErrStorageConflict
	}
	uc.transfers[hash]++
	var once sync.Once
	return func() {
		once.Do(func() {
			uc.repo.Lifecycle.Lock()
			defer uc.repo.Lifecycle.Unlock()
			uc.transfers[hash]--
			if uc.transfers[hash] == 0 {
				delete(uc.transfers, hash)
				uc.protectedUntil[hash] = time.Now().Add(engine.DebounceDelay)
			}
		})
	}, nil
}

func (uc *TorrentUseCase) MakePermanent(hash string) error {
	uc.repo.Lifecycle.Lock()
	defer uc.repo.Lifecycle.Unlock()
	found, err := uc.repo.MakePermanent(hash)
	if err != nil {
		return err
	}
	if !found {
		return TorrentNotFoundError{Hash: hash}
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
	if err := uc.repo.SaveTorrentWithPolicy(t, temporary); err != nil {
		if existing == nil {
			_ = uc.engine.Delete(t.Hash)
		}
		return nil, err
	}
	t.IsTemporary = temporary && (existing == nil || existing.IsTemporary)
	if temporary && existing == nil {
		uc.cleanupLocked("add")
	}
	return t, nil
}

func (uc *TorrentUseCase) SetTemporaryLimit(limit int) error {
	if limit < 1 {
		return fmt.Errorf("max temporary torrents must be positive")
	}
	uc.repo.Lifecycle.Lock()
	defer uc.repo.Lifecycle.Unlock()
	uc.maxTemporary = limit
	return nil
}

func (uc *TorrentUseCase) CleanupTemporary(reason string) {
	uc.repo.Lifecycle.Lock()
	defer uc.repo.Lifecycle.Unlock()
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
	for hash, until := range uc.protectedUntil {
		if time.Now().After(until) {
			delete(uc.protectedUntil, hash)
		}
	}
	queue, err := uc.repo.TemporaryQueue()
	if err != nil {
		logging.Warnf("temporary cleanup reason=%s: %v", reason, err)
		return
	}
	for len(queue) > uc.maxTemporary {
		hash := queue[0]
		if uc.transferProtected(hash) {
			logging.Debugf("temporary cleanup deferred reason=%s hash=%s active transfer or debounce", reason, hash)
			return
		}
		if err := uc.deleteTemporaryLocked(hash); err != nil {
			logging.Warnf("temporary cleanup failed reason=%s hash=%s: %v", reason, hash, err)
			return
		}
		delete(uc.protectedUntil, hash)
		logging.Infof("temporary cleanup removed reason=%s hash=%s", reason, hash)
		queue = queue[1:]
	}
}

func (uc *TorrentUseCase) transferProtected(hash string) bool {
	return uc.transfers[hash] > 0 || time.Now().Before(uc.protectedUntil[hash]) || uc.engine.StreamManager().ActiveStreamsForTorrent(hash) > 0
}
