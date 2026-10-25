package usecase

import (
	"encoding/json"
	"errors"
	"github.com/anacrolix/torrent/storage"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
)

func (uc *TorrentUseCase) attachmentFailure(t *models.Torrent, cause error) error {
	if cause == nil {
		return nil
	}
	rec, err := uc.repo.Recovery(t.Hash)
	if err != nil {
		return errors.Join(cause, err)
	}
	if rec != nil && (rec.Reason == models.StorageDeletionPending || rec.Reason == models.StorageDeletionCancelled || recoveryBlocks(rec)) {
		return cause
	}
	if rec == nil {
		rec = &repository.RecoveryRecord{Hash: t.Hash, RecordID: t.Control.RecordID, Version: 1}
	}
	rec.Reason = models.StorageAttachmentFailed
	rec.Error = "Runtime attachment failed; explicit verification/retry required"
	rec.Stage = "failed"
	var conflict *storage.LegacyPartConflictError
	if errors.As(cause, &conflict) {
		rec.Reason = models.StorageLegacyConflict
		rec.Error = "Legacy file conflict; both candidates preserved"
		rec.Stage = "conflict"
		journal, err := decodeLegacyJournal(rec)
		if err != nil {
			return errors.Join(cause, err)
		}
		journal.MetadataPath = conflict.Path
		journal.Analysis = nil
		journal.Choice = ""
		journal.Backup = ""
		data, err := json.Marshal(journal)
		if err != nil {
			return errors.Join(cause, err)
		}
		rec.Journal = string(data)
	}
	if err := uc.repo.SaveRecovery(rec); err != nil {
		return errors.Join(cause, err)
	}
	if conflict != nil {
		return &StorageOperationError{Code: models.StorageLegacyConflict, Hash: t.Hash, Cause: errors.Join(ErrStorageConflict, cause)}
	}
	return cause
}

func (uc *TorrentUseCase) RetryOwnership(hash, id string, version int64) error {
	t, err := uc.repo.GetTorrent(hash)
	if err != nil {
		return err
	}
	if t == nil || t.Control == nil || t.Control.RecordID != id || version != 1 {
		return ErrStorageConflict
	}
	if t.DeletionMode != "" {
		return ErrStorageConflict
	}
	// migrateStorage re-reads identity/mapping and never clears a blocked error
	// before a successful ownership check. It participates in the normal worker.
	select {
	case uc.storageWake <- struct{}{}:
	default:
	}
	return nil
}
