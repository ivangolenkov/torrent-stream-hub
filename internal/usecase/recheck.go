package usecase

import (
	"errors"

	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
)

func (uc *TorrentUseCase) Recheck(hash string) (*models.RecheckOperation, error) {
	uc.repo.Lifecycle.Lock()
	defer uc.repo.Lifecycle.Unlock()
	saved, err := uc.repo.GetTorrent(hash)
	if err != nil {
		return nil, err
	}
	if saved == nil {
		return nil, TorrentNotFoundError{Hash: hash}
	}
	if err := uc.priorityAvailable(hash); err != nil {
		return nil, err
	}
	if uc.transfers[hash] > 0 {
		return nil, ErrStorageConflict
	}
	if uc.engine.GetRawTorrent(hash) == nil {
		if saved.State != models.StatePaused {
			return nil, ErrStorageConflict
		}
		if err := uc.restoreInactiveLocked(saved); err != nil {
			return nil, err
		}
	}
	op, err := uc.engine.StartRecheck(hash)
	if errors.Is(err, engine.ErrTorrentNotFound) {
		return nil, TorrentNotFoundError{Hash: hash}
	}
	return op, err
}
