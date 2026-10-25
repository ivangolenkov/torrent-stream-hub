package usecase

import (
	"context"
	"errors"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
)

type StorageOperationError struct {
	Code  models.StorageRecoveryReason
	Hash  string
	Cause error
}

var ErrDeletionCancellation = repository.ErrDeletionCancelled

func (e *StorageOperationError) Error() string { return string(e.Code) }
func (e *StorageOperationError) Unwrap() error { return e.Cause }

func (uc *TorrentUseCase) recoveryExpected(hash, id string, version int64) (*repository.RecoveryRecord, error) {
	row, err := uc.repo.Recovery(hash)
	if err != nil {
		return nil, err
	}
	if row == nil || row.RecordID != id || row.Version != version {
		return nil, ErrStorageConflict
	}
	i, err := uc.repo.GetIntent(hash)
	if err != nil {
		return nil, err
	}
	if i == nil || i.Version.RecordID != id {
		return nil, ErrStorageConflict
	}
	return row, nil
}

func (uc *TorrentUseCase) RetryDeletion(ctx context.Context, hash, id string, version int64) error {
	release, err := uc.lockRecord(hash, ctx)
	if err != nil {
		return err
	}
	defer release()
	rec, err := uc.recoveryExpected(hash, id, version)
	if err != nil {
		return err
	}
	if rec.Reason != models.StorageDeletionPending || rec.Stage == "cancel_requested" {
		return ErrStorageConflict
	}
	t, err := uc.repo.GetTorrent(hash)
	if err != nil {
		return err
	}
	if t == nil {
		return ErrTorrentNotFound
	}
	if uc.transferProtected(hash) {
		return ErrStorageConflict
	}
	if t.DeletionMode == "payload" {
		return uc.deletePayloadLocked(t)
	}
	if t.DeletionMode != "keep" {
		return ErrStorageConflict
	}
	if err := uc.engine.Delete(hash); err != nil {
		return err
	}
	if err := uc.repo.DeletionGate(hash, id); err != nil {
		return err
	}
	return uc.repo.DeleteTorrent(hash)
}

func (uc *TorrentUseCase) CancelDeletion(ctx context.Context, hash, id string, version int64) error {
	if _, err := uc.recoveryExpected(hash, id, version); err != nil {
		return err
	}
	if err := uc.repo.RequestDeletionCancel(hash, id, version); err != nil {
		return err
	}
	// No lock held while joining the synchronous worker. Cancellation of this
	// wait leaves the durable request for startup/the next explicit cancel.
	release, err := uc.lockRecord(hash, ctx)
	if err != nil {
		return err
	}
	defer release()
	if _, err := uc.recoveryExpected(hash, id, version); err != nil {
		return err
	}
	if err := uc.engine.Delete(hash); err != nil {
		return err
	}
	return uc.repo.CompleteDeletionCancel(hash, id, version)
}

func (uc *TorrentUseCase) ForgetRecovered(ctx context.Context, hash, id string, version int64) error {
	release, err := uc.lockRecord(hash, ctx)
	if err != nil {
		return err
	}
	defer release()
	rec, err := uc.recoveryExpected(hash, id, version)
	if err != nil {
		return err
	}
	if rec.Reason != models.StorageDeletionCancelled || rec.Stage != "cancelled" {
		return ErrStorageConflict
	}
	if uc.transferProtected(hash) {
		return ErrStorageConflict
	}
	if err := uc.repo.BeginForget(hash); err != nil {
		return err
	}
	if err := uc.engine.Delete(hash); err != nil {
		return err
	}
	return uc.repo.DeleteTorrent(hash)
}

func isDeletionCancellation(err error) bool { return errors.Is(err, repository.ErrDeletionCancelled) }
