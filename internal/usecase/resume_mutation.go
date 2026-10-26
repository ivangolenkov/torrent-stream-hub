package usecase

import (
	"context"
	"errors"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"torrent-stream-hub/internal/fastresume"
	"torrent-stream-hub/internal/repository"
)

func (uc *TorrentUseCase) resumeMutationHooks(openCtx context.Context, info *metainfo.Info, hash metainfo.Hash) (result *storage.FileResumeHooks, resultErr error) {
	defer func() {
		if result != nil {
			result.Observation.Mode = string(uc.resumeMode)
			if uc.resumeMode == fastresume.Verify {
				result.Observation.Status, result.Observation.Reason = "disabled", "strict_mode"
			}
			if result.Observation.Status == "" {
				result.Observation.Status, result.Observation.Reason = "fallback", "no_checkpoint"
			}
		}
	}()
	m, err := uc.repo.StorageMapping(hash.HexString())
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, ErrStorageConflict
	}
	owner, record, err := uc.repo.ResumeOwnerMapping(uc.engine.Context(), m.StorageID)
	if errors.Is(err, repository.ErrStateConflict) {
		safe, probeErr := uc.repo.ResumeUnboundMutationSafe(uc.engine.Context(), m)
		if probeErr != nil {
			return nil, probeErr
		}
		if !safe {
			return nil, err
		}
		if info.NumPieces() < 0 {
			return nil, fastresume.ErrInvalid
		}
		if info.NumPieces() > fastresume.MaxPieces {
			return nil, nil
		}
		// Registration has no durable record yet. Bind once at the first
		// later safety operation; Drop joins this backend before re-add.
		boundRecord := ""
		resolve := func() (*repository.StorageMapping, string, error) {
			current, id, err := uc.repo.ResumeOwnerMapping(uc.engine.Context(), m.StorageID)
			if err != nil {
				return nil, "", err
			}
			if current.Layout() != m.Layout() || current.Revision != m.Revision || boundRecord != "" && boundRecord != id {
				return nil, "", ErrStorageConflict
			}
			boundRecord = id
			return current, id, nil
		}
		return &storage.FileResumeHooks{Pending: make([]byte, (info.NumPieces()+7)/8),
			Revoke: func(mask []byte) (bool, error) {
				current, id, err := resolve()
				if err != nil {
					return false, err
				}
				return uc.repo.RevokeResumeMutation(uc.engine.Context(), current, id, mask)
			},
			Invalidate: func() error {
				current, id, err := resolve()
				if errors.Is(err, repository.ErrStateConflict) {
					safe, probeErr := uc.repo.ResumeUnboundMutationSafe(uc.engine.Context(), m)
					if probeErr != nil {
						return probeErr
					}
					if safe {
						return nil
					}
				}
				if err != nil {
					return err
				}
				return uc.repo.InvalidateResumeMutation(uc.engine.Context(), current, id)
			},
		}, nil
	}
	if err != nil {
		return nil, err
	}
	if owner.Layout() != m.Layout() || owner.Revision != m.Revision {
		return nil, ErrStorageConflict
	}
	if uc.resumeMode != fastresume.Fast {
		return uc.repo.NewResumeMutationHooks(uc.engine.Context(), owner, record, info.NumPieces(), info.PieceLength, true)
	}
	hooks, err := uc.repo.NewResumeMutationHooks(uc.engine.Context(), owner, record, info.NumPieces(), info.PieceLength, false)
	if err != nil {
		// Unavailable optimization (e.g. an active recovery journal) must not
		// prevent ordinary verification. Strict untrust still fences this exact
		// mapping/record and must succeed before that fallback is allowed.
		fallback, fallbackErr := uc.repo.NewResumeMutationHooks(uc.engine.Context(), owner, record, info.NumPieces(), info.PieceLength, true)
		if fallback != nil {
			fallback.Observation.Status, fallback.Observation.Reason = "fallback", resumeFailureReason(err)
		}
		return fallback, fallbackErr
	}
	if hooks == nil || !hooks.RestoreCandidate {
		return hooks, nil
	}
	source, err := uc.resumeOpenSource(openCtx, owner, record, info)
	if err != nil {
		hooks.Observation.Status, hooks.Observation.Reason = "fallback", resumeFailureReason(err)
		if err := hooks.Invalidate(); err != nil {
			return nil, err
		}
		clear(hooks.Pending)
		// Preserve candidate status: failed restore must not manufacture an
		// absent empty file. Verification may proceed without that readiness.
		return hooks, nil
	}
	hooks.Restore = func(ctx context.Context, backend storage.TorrentImpl) error {
		if err := uc.repo.RestoreResume(ctx, owner, source, backend); err != nil {
			hooks.Observation.Status, hooks.Observation.Reason = "fallback", resumeFailureReason(err)
			// Prepare/import failures never grant readiness. A failed durable
			// fallback barrier fails the open rather than admitting mutations.
			return hooks.Invalidate()
		}
		restored, expected := 0, 0
		for i := 0; i < info.NumPieces(); i++ {
			if hooks.Pending[i/8]&(1<<uint(i%8)) != 0 {
				expected++
			}
			if backend.CachedRestored != nil && backend.CachedRestored(i) {
				restored++
			}
		}
		hooks.Observation.Status = "restored"
		if restored < info.NumPieces() {
			hooks.Observation.Status = "partial"
		}
		if restored == 0 && info.NumPieces() > 0 {
			hooks.Observation.Status = "fallback"
		}
		if restored < expected {
			hooks.Observation.Reason = "files_changed"
		}
		return nil
	}
	return hooks, nil
}

func resumeFailureReason(err error) string {
	switch {
	case errors.Is(err, fastresume.ErrBudget), errors.Is(err, storage.ErrCheckpointBudget):
		return "budget_exceeded"
	case errors.Is(err, repository.ErrResumeIntegrity):
		return "checkpoint_integrity_error"
	case errors.Is(err, fastresume.ErrInvalid):
		return "invalid_checkpoint"
	case errors.Is(err, storage.ErrCheckpointUnsupported):
		return "unsupported_storage"
	case errors.Is(err, storage.ErrCheckpointChanged):
		return "files_changed"
	case errors.Is(err, ErrStorageConflict), errors.Is(err, repository.ErrStateConflict):
		return "storage_blocked"
	default:
		return "checkpoint_io_error"
	}
}

// Caller owns the existing command/storage mutation reservation. This barrier
// deliberately works with active journals/deletion intent and does no FS I/O.
func (uc *TorrentUseCase) revokeResumeMutation(ctx context.Context, hash string) error {
	m, err := uc.repo.StorageMapping(hash)
	if err != nil || m == nil {
		return err
	}
	owner, record, err := uc.repo.ResumeOwnerMapping(ctx, m.StorageID)
	if err != nil {
		return err
	}
	return uc.repo.InvalidateResumeMutation(ctx, owner, record)
}
