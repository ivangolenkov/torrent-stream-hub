package usecase

import (
	"context"
	"errors"
	"github.com/anacrolix/torrent"

	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
)

// Caller owns record/root handles. Attachment is issued to the captured
// pointer, not to whichever runtime happens to exist after a database roundtrip.
// Commands call this only after a successful apply. Sync may initialize only an
// unbound legacy runtime, never silently promote its applied intent revision.
func bindAppliedRuntime(e *engine.Engine, r *repository.TorrentRepo, v models.IntentVersion) error {
	expected, old, applied := e.RuntimeAppliedBinding(v.Hash)
	if expected == nil {
		return nil
	}
	if old.IntentVersion == v && old.Attachment != "" && applied == v.Revision {
		return nil
	}
	f, err := r.AttachRuntime(v)
	if err != nil {
		return err
	}
	return e.BindRuntime(expected, f)
}

// Metadata/policy/selection apply only their own change. They must not acknowledge
// an earlier failed run/selection command merely by binding the latest revision.
// Explicit Pause/Resume and restore perform full reconciliation separately.
func bindIncrementalRuntime(e *engine.Engine, r *repository.TorrentRepo, v models.IntentVersion) error {
	expected, old, applied := e.RuntimeAppliedBinding(v.Hash)
	if expected == nil {
		return nil
	}
	if old.RecordID != v.RecordID || old.Revision != v.Revision-1 || applied != old.Revision {
		return nil
	}
	return bindAppliedRuntime(e, r, v)
}

func bindVerificationRuntime(e *engine.Engine, r *repository.TorrentRepo, v models.IntentVersion) error {
	expected, old := e.RuntimeBinding(v.Hash)
	if expected == nil {
		return engine.ErrTorrentNotFound
	}
	if old.IntentVersion == v && old.Attachment != "" {
		return nil
	}
	f, err := r.AttachRuntime(v)
	if err != nil {
		return err
	}
	// Recheck applies verification, not a pending user command.
	return e.BindObservedRuntime(expected, f)
}

func capturePersistableRuntime(e *engine.Engine, r *repository.TorrentRepo, hash string, contexts ...context.Context) (*models.Torrent, error) {
	expected, f := e.RuntimeBinding(hash)
	if expected == nil {
		return nil, engine.ErrTorrentNotFound
	}
	if f.Attachment == "" {
		i, err := r.GetIntent(hash)
		if err != nil {
			return nil, err
		}
		if i == nil {
			return nil, ErrTorrentNotFound
		}
		f, err = r.AttachRuntime(i.Version)
		if err != nil {
			return nil, err
		}
		if err := e.BindObservedRuntime(expected, f); err != nil {
			return nil, err
		}
	}
	t, o, err := e.CaptureObservation(hash)
	if err != nil {
		return nil, err
	}
	if t.MetadataKnown {
		catalog := runtimeCatalog(t)
		if err := r.SaveCatalog(o.Fence, catalog, contexts...); err != nil {
			return nil, err
		}
	}
	if err := r.SaveRuntimeObservation(o, contexts...); err != nil {
		return nil, err
	}
	return t, nil
}

func runtimeCatalog(t *models.Torrent) models.TorrentCatalog {
	c := models.TorrentCatalog{Format: t.MetadataFormat, Name: t.Name, Size: t.Size, PieceLength: t.PieceLength, NumPieces: t.NumPieces}
	for _, f := range t.Files {
		c.Files = append(c.Files, models.FileCatalog{Index: f.Index, Path: f.Path, Size: f.Size, Offset: f.Offset, IsMedia: f.IsMedia})
	}
	return c
}

func (uc *TorrentUseCase) currentIntent(hash string) (*models.TorrentIntent, error) {
	i, err := uc.repo.GetIntent(hash)
	if err != nil {
		return nil, err
	}
	if i == nil {
		return nil, TorrentNotFoundError{Hash: hash}
	}
	return i, nil
}

func commandStateError(err error) error {
	if errors.Is(err, repository.ErrStateConflict) {
		return errors.Join(ErrStorageConflict, err)
	}
	return err
}

// Never look up a replacement runtime by hash when reporting a late failure.
// No retry loop/queue is installed: an explicit full Pause/Resume or reopen is
// the bounded recovery contract. Samplers and incremental commands cannot heal it.
func (uc *TorrentUseCase) failedIntentApply(expected *torrent.Torrent, v models.IntentVersion, code string, err error) error {
	if expected != nil {
		err = errors.Join(err, uc.engine.FailIntentApply(expected, v, code))
		blocks, captureErr := uc.engine.RuntimeSafetyBlocks(expected, v.Hash)
		err = errors.Join(err, captureErr)
		if captureErr == nil && len(blocks) > 0 {
			_, f := uc.engine.RuntimeBinding(v.Hash)
			if f.RecordID == v.RecordID && f.Attachment != "" {
				for _, block := range blocks {
					err = errors.Join(err, uc.repo.PutRuntimeSafetyBlock(f, block.Kind, block.Reason))
				}
			} else {
				// New Add/restore may fault after create/commit but before its
				// first binding. Persist uncertainty now, never after success or
				// an eventual sampler tick; this does not acknowledge apply.
				f, attachErr := uc.repo.AttachRuntime(v)
				err = errors.Join(err, attachErr)
				if attachErr == nil {
					err = errors.Join(err, uc.engine.BindObservedRuntime(expected, f))
				}
			}
		}
	}
	return commandStateError(err)
}

// Record/root handles exclude another command between this read and the
// revisioned priority write. The write advances exactly once or rolls back (also
// at MaxInt64). A04 must retain this contract with per-record serialization/CAS.
func nextSavedVersion(saved *models.Torrent) models.IntentVersion {
	return models.IntentVersion{Hash: saved.Hash, RecordID: saved.Control.RecordID, Revision: saved.Control.DesiredRevision + 1}
}
