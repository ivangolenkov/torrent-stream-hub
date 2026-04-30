package usecase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"torrent-stream-hub/internal/engine"
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
	t, err := uc.engine.AddMagnet(magnet)
	if err != nil {
		return nil, err
	}

	// Save initial state to DB
	if err := uc.repo.SaveTorrent(t); err != nil {
		// Log error, but don't fail the add operation
		// A proper logger should be used here
	}

	return t, nil
}

func (uc *TorrentUseCase) AddTorrentFile(r io.Reader) (*models.Torrent, error) {
	t, err := uc.engine.AddTorrentFile(r)
	if err != nil {
		return nil, err
	}

	if err := uc.repo.SaveTorrent(t); err != nil {
		// Keep the torrent in the engine even if DB persistence fails.
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
	t, err := uc.repo.GetTorrent(hash)
	if err != nil {
		return err
	}
	if t == nil {
		return TorrentNotFoundError{Hash: hash}
	}

	if err := uc.engine.Pause(hash); err != nil && !errors.Is(err, engine.ErrTorrentNotFound) {
		return err
	}

	t.State = models.StatePaused
	return uc.repo.SaveTorrent(t)
}

func (uc *TorrentUseCase) Resume(hash string) error {
	t, err := uc.repo.GetTorrent(hash)
	if err != nil {
		return err
	}
	if t == nil {
		return TorrentNotFoundError{Hash: hash}
	}

	if err := uc.engine.Resume(hash); err != nil {
		if !errors.Is(err, engine.ErrTorrentNotFound) {
			return err
		}
		if _, err := uc.engine.AddInfoHash(hash); err != nil {
			return err
		}
	}

	t.State = models.StateQueued
	t.Error = models.ErrNone
	return uc.repo.SaveTorrent(t)
}

func (uc *TorrentUseCase) Delete(hash string, deleteFiles bool) error {
	t, err := uc.repo.GetTorrent(hash)
	if err != nil {
		return err
	}
	if t == nil {
		return TorrentNotFoundError{Hash: hash}
	}

	if err := uc.engine.Delete(hash); err != nil {
		return err
	}

	// Delete from DB
	if err := uc.repo.DeleteTorrent(hash); err != nil {
		return err
	}

	// TODO: Actually delete files from disk if deleteFiles is true
	// We might need to ask the engine for the download dir and construct the path

	return nil
}

func (uc *TorrentUseCase) RestoreTorrents() error {
	torrents, err := uc.repo.GetAllTorrents()
	if err != nil {
		return err
	}

	var restoreErr error
	for _, t := range torrents {
		switch t.State {
		case models.StatePaused, models.StateError, models.StateMissingFiles:
			continue
		}

		if _, err := uc.engine.AddInfoHash(t.Hash); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore torrent %s: %w", t.Hash, err))
		}
	}

	return restoreErr
}

func (uc *TorrentUseCase) AddStream(ctx context.Context, hash string, index int) error {
	if err := uc.engine.StreamManager().AddStream(ctx, hash, index); err != nil {
		if errors.Is(err, engine.ErrTorrentNotFound) {
			return TorrentNotFoundError{Hash: hash}
		}
		return err
	}
	return nil
}

func (uc *TorrentUseCase) GetTorrentFile(hash string, index int) (*torrent.File, error) {
	f, err := uc.engine.GetTorrentFile(hash, index)
	if err != nil {
		if errors.Is(err, engine.ErrTorrentNotFound) {
			return nil, TorrentNotFoundError{Hash: hash}
		}
		return nil, err
	}
	return f, nil
}
