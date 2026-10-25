package usecase

import (
	"context"
	"errors"
	"github.com/anacrolix/torrent/storage"
	"os"
	"syscall"
	"time"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"
)

type activeLocationWork struct {
	hash, id string
	version  int64
	cancel   context.CancelCauseFunc
}

type storageMutationHeld struct{}

func (uc *TorrentUseCase) cancelLocation(hash, id string, version int64) {
	uc.locationWorkMu.Lock()
	defer uc.locationWorkMu.Unlock()
	if w := uc.locationWork; w != nil && w.hash == hash && w.id == id && w.version == version {
		w.cancel(errLocationCancelled)
	}
}

func (uc *TorrentUseCase) runLocationOperations(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		hashes, err := uc.repo.PendingStorageOperations(ctx)
		if err == nil {
			for _, hash := range hashes {
				if ctx.Err() != nil {
					return
				}
				uc.processLocation(ctx, hash)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-uc.locationWake:
		}
	}
}

func (uc *TorrentUseCase) processLocation(parent context.Context, hash string) {
	ticket, err := uc.coordinator.tryAcquire("storage/mutation")
	if err != nil {
		return
	}
	defer ticket.release()
	release, err := uc.tryLockRecord(hash)
	if err != nil {
		return
	}
	rec, err := uc.repo.Recovery(hash)
	if err != nil || rec == nil || (rec.Reason != models.StorageMove && rec.Reason != models.StorageLocate) || rec.Error != "" {
		release()
		return
	}
	m, err := uc.repo.StorageMapping(hash)
	if err != nil || m == nil {
		release()
		return
	}
	t, err := uc.repo.GetTorrent(hash)
	if err != nil || t == nil || t.Control.RecordID != rec.RecordID || t.DeletionMode != "" {
		release()
		return
	}
	j, err := decodeLocationJournal(rec)
	if err != nil || !locationMappingMatches(m, j, rec.Stage) {
		release()
		uc.locationFailure(m, rec, ErrStorageConflict)
		return
	}
	ctx, cancel := context.WithCancelCause(parent)
	w := &activeLocationWork{hash: hash, id: rec.RecordID, version: rec.Version, cancel: cancel}
	uc.locationWorkMu.Lock()
	uc.locationWork = w
	uc.locationWorkMu.Unlock()
	release()
	defer func() {
		cancel(nil)
		uc.locationWorkMu.Lock()
		if uc.locationWork == w {
			uc.locationWork = nil
		}
		uc.locationWorkMu.Unlock()
	}()
	done := uc.startStorageProgress(hash, rec.RecordID, rec.Version, rec.Reason)
	defer done()
	root, err := os.OpenRoot(uc.engine.DownloadDir())
	if err == nil {
		defer root.Close()
		err = uc.checkLocationVolumes(j, rec.Reason == models.StorageLocate)
		if err != nil { /* Persist the unavailable volume as the operation failure. */
		} else if rec.Reason == models.StorageLocate {
			err = uc.executeLocate(ctx, root, m, rec, j)
		} else {
			err = uc.executeLocation(ctx, root, m, rec, j)
		}
	}
	current, readErr := uc.repo.Recovery(hash)
	if parent.Err() != nil || readErr != nil || current == nil || current.RecordID != rec.RecordID || current.Version != rec.Version || current.Reason != rec.Reason {
		return
	}
	if current.Stage == "cancel_requested" {
		uc.finishLocationCancel(parent, root, m, current, j)
		return
	}
	if err != nil {
		uc.locationFailure(m, current, err)
	}
}

func (uc *TorrentUseCase) checkLocationVolumes(j *storageLocationJournal, locate ...bool) error {
	paths := []string{j.Target.Root, j.Staging}
	if len(locate) == 0 || !locate[0] {
		paths = append(paths, j.Source.Root)
	}
	for _, path := range paths {
		if err := uc.checkVolume(path); err != nil {
			return err
		}
	}
	return nil
}

func (uc *TorrentUseCase) locationFailure(m *repository.StorageMapping, v *repository.RecoveryRecord, cause error) {
	// Preserve the last durable stage: a syscall may have succeeded before its
	// error/flush/checkpoint failed. Retry inspects both names at that stage.
	if errors.Is(cause, context.Canceled) {
		return
	}
	current, err := uc.repo.StorageMapping(v.Hash)
	if err != nil || current == nil || current.StorageID != m.StorageID {
		return
	}
	v.Error = cause.Error()
	v.FailureCode = "io_error"
	switch {
	case errors.Is(cause, payload.ErrVolumeUnavailable):
		v.FailureCode = "volume_unavailable"
	case errors.Is(cause, payload.ErrVolumeUnregistered):
		v.FailureCode = "volume_unregistered"
	case errors.Is(cause, ErrRecoveryCandidateChanged):
		v.FailureCode = "candidate_changed"
	case errors.Is(cause, os.ErrExist):
		v.FailureCode = "target_exists"
	case errors.Is(cause, engine.ErrMigrationDiskFull) || errors.Is(cause, syscall.ENOSPC):
		v.FailureCode = "disk_full"
	case errors.Is(cause, storage.ErrPublicationFlush):
		v.FailureCode = "flush_failed"
	case errors.Is(cause, storage.ErrUnsafeFile) || errors.Is(cause, payload.ErrUnsafePath):
		v.FailureCode = "unsafe_candidate"
	case errors.Is(cause, ErrStorageConflict) || errors.Is(cause, repository.ErrStateConflict):
		v.FailureCode = "state_conflict"
	}
	if err := uc.repo.TransitionStorageOperation(current, v, v.Stage); err == nil {
		_ = uc.recoveryBoundary("location_failed")
	}
}

func (uc *TorrentUseCase) waitLocationReaders(ctx context.Context, hash string) error {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for uc.transferProtected(hash) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
	return ctx.Err()
}

func (uc *TorrentUseCase) restoreAfterLocation(ctx context.Context, v *repository.RecoveryRecord) error {
	release, err := uc.lockRecord(v.Hash, ctx)
	if err != nil {
		return err
	}
	defer release()
	current, err := uc.repo.GetTorrent(v.Hash)
	if err != nil {
		return err
	}
	if current == nil || current.Control.RecordID != v.RecordID {
		return ErrStorageConflict
	}
	err = uc.restoreMappedTorrentLocked(current)
	if errors.Is(err, ErrInventoryPending) {
		uc.wakeInventory()
		return nil
	}
	return err
}
