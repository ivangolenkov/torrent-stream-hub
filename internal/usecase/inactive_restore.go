package usecase

import (
	"context"
	"path/filepath"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"torrent-stream-hub/internal/models"
)

// Caller holds Lifecycle. Only cached, hash-matching metainfo may open an
// inactive runtime: never fall back to network metadata acquisition here.
func (uc *TorrentUseCase) restoreInactiveLocked(saved *models.Torrent) error {
	mi, err := metainfo.LoadFromFile(filepath.Join(uc.engine.ConfigDir(), "metainfo", saved.Hash+".torrent"))
	if err != nil {
		return ErrMetadataPending
	}
	spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		return ErrMetadataPending
	}
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
	runtime := uc.engine.GetTorrent(hash)
	if runtime.State == models.StatePaused {
		if ready, _, err := uc.engine.CheckFileDirectReady(hash, index); err != nil {
			return err
		} else if !ready {
			return uc.engine.VerifyInactive(ctx, hash)
		}
	}
	return nil
}
