package usecase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"

	"github.com/anacrolix/torrent"
)

var ErrTorrentNotFound = errors.New("torrent not found")

type TorrentNotFoundError struct {
	Hash string
}

func (e TorrentNotFoundError) Error() string {
	return fmt.Sprintf("torrent not found: %s", e.Hash)
}

func (e TorrentNotFoundError) Is(target error) bool {
	return target == ErrTorrentNotFound
}

type TorrentUseCase struct {
	engine *engine.Engine
	repo   *repository.TorrentRepo
}

func NewTorrentUseCase(e *engine.Engine, r *repository.TorrentRepo) *TorrentUseCase {
	return &TorrentUseCase{
		engine: e,
		repo:   r,
	}
}

func (uc *TorrentUseCase) AddMagnet(magnet string) (*models.Torrent, error) {
	logging.Infof("usecase add magnet %s", logging.SafeMagnetSummary(magnet))
	t, err := uc.engine.AddMagnet(magnet)
	if err != nil {
		logging.Warnf("usecase add magnet failed %s: %v", logging.SafeMagnetSummary(magnet), err)
		return nil, err
	}

	// Save initial state to DB
	if err := uc.repo.SaveTorrent(t); err != nil {
		logging.Warnf("failed to persist torrent after magnet add hash=%s: %v", t.Hash, err)
	}

	return t, nil
}

func (uc *TorrentUseCase) AddTorrentFile(r io.Reader) (*models.Torrent, error) {
	logging.Infof("usecase add .torrent file")
	t, err := uc.engine.AddTorrentFile(r)
	if err != nil {
		logging.Warnf("usecase add .torrent file failed: %v", err)
		return nil, err
	}

	if err := uc.repo.SaveTorrent(t); err != nil {
		logging.Warnf("failed to persist torrent after file add hash=%s: %v", t.Hash, err)
	}

	return t, nil
}

func (uc *TorrentUseCase) GetAllTorrents() ([]*models.Torrent, error) {
	dbTorrents, err := uc.repo.GetAllTorrents()
	if err != nil {
		return nil, err
	}

	engineTorrents := uc.engine.GetAllTorrents()
	if len(engineTorrents) == 0 {
		return dbTorrents, nil
	}

	engineByHash := make(map[string]*models.Torrent, len(engineTorrents))
	for _, t := range engineTorrents {
		engineByHash[t.Hash] = t
	}

	merged := make([]*models.Torrent, 0, len(dbTorrents)+len(engineTorrents))
	seen := make(map[string]bool, len(dbTorrents)+len(engineTorrents))
	for _, dbT := range dbTorrents {
		if engineT, ok := engineByHash[dbT.Hash]; ok {
			merged = append(merged, engineT)
		} else {
			merged = append(merged, dbT)
		}
		seen[dbT.Hash] = true
	}

	for _, engineT := range engineTorrents {
		if !seen[engineT.Hash] {
			merged = append(merged, engineT)
		}
	}

	return merged, nil
}

func (uc *TorrentUseCase) GetTorrent(hash string) (*models.Torrent, error) {
	return uc.repo.GetTorrent(hash)
}

func (uc *TorrentUseCase) Pause(hash string) error {
	logging.Infof("usecase pause hash=%s", hash)
	t, err := uc.repo.GetTorrent(hash)
	if err != nil {
		logging.Warnf("pause failed to load torrent hash=%s: %v", hash, err)
		return err
	}
	if t == nil {
		logging.Debugf("pause requested for missing torrent hash=%s", hash)
		return TorrentNotFoundError{Hash: hash}
	}

	if err := uc.engine.Pause(hash); err != nil && !errors.Is(err, engine.ErrTorrentNotFound) {
		logging.Warnf("engine pause failed hash=%s: %v", hash, err)
		return err
	} else if err != nil {
		logging.Debugf("pause applied to DB-only torrent hash=%s", hash)
	}

	t.State = models.StatePaused
	if err := uc.repo.SaveTorrent(t); err != nil {
		logging.Warnf("failed to persist paused state hash=%s: %v", hash, err)
		return err
	}
	return nil
}

func (uc *TorrentUseCase) Resume(hash string) error {
	logging.Infof("usecase resume hash=%s", hash)
	t, err := uc.repo.GetTorrent(hash)
	if err != nil {
		logging.Warnf("resume failed to load torrent hash=%s: %v", hash, err)
		return err
	}
	if t == nil {
		logging.Debugf("resume requested for missing torrent hash=%s", hash)
		return TorrentNotFoundError{Hash: hash}
	}

	if err := uc.engine.Resume(hash); err != nil {
		if !errors.Is(err, engine.ErrTorrentNotFound) {
			logging.Warnf("engine resume failed hash=%s: %v", hash, err)
			return err
		}
		logging.Infof("resume restoring DB-only torrent hash=%s source_present=%t", hash, t.SourceURI != "")
		restored, err := uc.restoreTorrentToEngine(t)
		if err != nil {
			logging.Warnf("resume restore failed hash=%s: %v", hash, err)
			return err
		}
		if t.SourceURI == "" {
			t.SourceURI = restored.SourceURI
		}
	}

	t.State = models.StateQueued
	t.Error = models.ErrNone
	if err := uc.repo.SaveTorrent(t); err != nil {
		logging.Warnf("failed to persist resumed state hash=%s: %v", hash, err)
		return err
	}
	return nil
}

func (uc *TorrentUseCase) Delete(hash string, deleteFiles bool) error {
	logging.Infof("usecase delete hash=%s delete_files=%t", hash, deleteFiles)
	t, err := uc.repo.GetTorrent(hash)
	if err != nil {
		logging.Warnf("delete failed to load torrent hash=%s: %v", hash, err)
		return err
	}
	if t == nil {
		logging.Debugf("delete requested for missing torrent hash=%s", hash)
		return TorrentNotFoundError{Hash: hash}
	}

	if err := uc.engine.Delete(hash); err != nil {
		logging.Warnf("engine delete failed hash=%s: %v", hash, err)
		return err
	}

	// Delete from DB
	if err := uc.repo.DeleteTorrent(hash); err != nil {
		logging.Warnf("failed to delete torrent from DB hash=%s: %v", hash, err)
		return err
	}

	// TODO: Actually delete files from disk if deleteFiles is true
	// We might need to ask the engine for the download dir and construct the path

	return nil
}

func (uc *TorrentUseCase) RestoreTorrents() error {
	logging.Infof("restoring persisted torrents")
	torrents, err := uc.repo.GetAllTorrents()
	if err != nil {
		logging.Warnf("failed to load torrents for restore: %v", err)
		return err
	}

	var restoreErr error
	for _, t := range torrents {
		switch t.State {
		case models.StatePaused, models.StateError, models.StateMissingFiles:
			logging.Debugf("skipping restore hash=%s state=%s", t.Hash, t.State)
			continue
		}

		logging.Infof("restoring torrent hash=%s state=%s source_present=%t", t.Hash, t.State, t.SourceURI != "")
		restored, err := uc.restoreTorrentToEngine(t)
		if err == nil {
			if err := uc.repo.SaveTorrent(restored); err != nil {
				logging.Warnf("failed to save restored torrent hash=%s: %v", t.Hash, err)
				restoreErr = errors.Join(restoreErr, fmt.Errorf("save restored torrent %s: %w", t.Hash, err))
			}
			continue
		}
		if err != nil {
			logging.Warnf("failed to restore torrent hash=%s: %v", t.Hash, err)
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore torrent %s: %w", t.Hash, err))
		}
	}

	return restoreErr
}

func (uc *TorrentUseCase) restoreTorrentToEngine(t *models.Torrent) (*models.Torrent, error) {
	if t.SourceURI != "" {
		logging.Debugf("restore uses persisted source URI hash=%s", t.Hash)
		return uc.engine.AddMagnet(t.SourceURI)
	}

	logging.Warnf("restore falling back to bare info hash hash=%s", t.Hash)
	return uc.engine.AddInfoHash(t.Hash)
}

func (uc *TorrentUseCase) AddStream(ctx context.Context, hash string, index int) error {
	logging.Debugf("usecase add stream hash=%s file_index=%d", hash, index)
	if err := uc.engine.StreamManager().AddStream(ctx, hash, index); err != nil {
		if errors.Is(err, engine.ErrTorrentNotFound) {
			logging.Debugf("stream requested for missing torrent hash=%s file_index=%d", hash, index)
			return TorrentNotFoundError{Hash: hash}
		}
		logging.Warnf("failed to add stream hash=%s file_index=%d: %v", hash, index, err)
		return err
	}
	return nil
}

func (uc *TorrentUseCase) GetTorrentFile(hash string, index int) (*torrent.File, error) {
	logging.Debugf("usecase get torrent file hash=%s file_index=%d", hash, index)
	f, err := uc.engine.GetTorrentFile(hash, index)
	if err != nil {
		if errors.Is(err, engine.ErrTorrentNotFound) {
			logging.Debugf("get torrent file missing torrent hash=%s file_index=%d", hash, index)
			return nil, TorrentNotFoundError{Hash: hash}
		}
		logging.Warnf("get torrent file failed hash=%s file_index=%d: %v", hash, index, err)
		return nil, err
	}
	return f, nil
}
