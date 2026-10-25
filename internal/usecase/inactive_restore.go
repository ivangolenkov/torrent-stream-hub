package usecase

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"torrent-stream-hub/internal/models"
)

// Caller owns record/root handles. Only cached, hash-matching metainfo may open an
// inactive runtime: never fall back to network metadata acquisition here.
func (uc *TorrentUseCase) restoreInactiveLocked(saved *models.Torrent) (retErr error) {
	defer func() {
		if retErr != nil && !errors.Is(retErr, ErrMetadataPending) {
			retErr = uc.attachmentFailure(saved, retErr)
		}
	}()
	mi, err := metainfo.LoadFromFile(filepath.Join(uc.engine.ConfigDir(), "metainfo", saved.Hash+".torrent"))
	if err != nil {
		return ErrMetadataPending
	}
	spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		return ErrMetadataPending
	}
	// Blocked DiskFull may also carry a verification/legacy key: display phase
	// must not accidentally decide whether this local-only restore can network.
	spec.DisableNetworking = true
	if _, err := uc.engine.RestoreTorrent(spec, saved); err != nil {
		return err
	}
	return uc.engine.FinishRestore(saved.Hash, saved.State)
}

func (uc *TorrentUseCase) prepareReadLocked(ctx context.Context, hash string, index int) error {
	saved, err := uc.repo.GetTorrent(hash)
	if err != nil {
		return err
	}
	if saved == nil {
		return TorrentNotFoundError{Hash: hash}
	}
	if err := uc.priorityAvailable(hash); err != nil {
		return err
	}
	if uc.engine.GetRawTorrent(hash) == nil {
		if saved.State != models.StatePaused {
			return ErrStorageConflict
		}
		if err := uc.restoreInactiveLocked(saved); err != nil {
			return err
		}
	}
	// Bind observations before read-time safety detection. An inactive runtime
	// can be created here without a preceding user command; its physical faults
	// still need a record/attachment capability before returning to the client.
	i, err := uc.currentIntent(hash)
	if err != nil {
		return err
	}
	if err := bindVerificationRuntime(uc.engine, uc.repo, i.Version); err != nil {
		return commandStateError(err)
	}
	runtime := uc.engine.GetTorrent(hash)
	if runtime.State == models.StatePaused {
		if ready, _, err := uc.engine.CheckFileDirectReady(hash, index); err != nil {
			return err
		} else if !ready {
			i, err := uc.currentIntent(hash)
			if err != nil {
				return err
			}
			if err := bindVerificationRuntime(uc.engine, uc.repo, i.Version); err != nil {
				return commandStateError(err)
			}
			verify := uc.inactiveVerify
			if verify == nil {
				verify = uc.engine.VerifyInactive
			}
			return verify(ctx, hash)
		}
	}
	return nil
}
