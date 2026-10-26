package usecase

import (
	"context"
	"errors"

	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
)

func (uc *TorrentUseCase) Recheck(hash string, contexts ...context.Context) (*models.RecheckOperation, error) {
	release, err := uc.lockRecord(hash, contexts...)
	if err != nil {
		return nil, err
	}
	defer release()
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
	if uc.transferCount(hash) > 0 {
		return nil, ErrStorageConflict
	}
	if err := uc.revokeResumeMutation(uc.requestContext(contexts...), hash); err != nil {
		return nil, err
	}
	if uc.engine.GetRawTorrent(hash) == nil {
		if saved.State != models.StatePaused {
			return nil, ErrStorageConflict
		}
		if err := uc.restoreInactiveLocked(saved); err != nil {
			return nil, err
		}
	}
	i, err := uc.currentIntent(hash)
	if err != nil {
		return nil, err
	}
	if err := bindVerificationRuntime(uc.engine, uc.repo, i.Version); err != nil {
		return nil, commandStateError(err)
	}
	op, err := uc.engine.StartRecheck(hash)
	if errors.Is(err, engine.ErrTorrentNotFound) {
		return nil, TorrentNotFoundError{Hash: hash}
	}
	return op, err
}
