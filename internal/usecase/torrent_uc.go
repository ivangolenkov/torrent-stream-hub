package usecase

import (
	"context"
	"io"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"

	"github.com/anacrolix/torrent"
)

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
	return uc.repo.GetAllTorrents()
}

func (uc *TorrentUseCase) GetTorrent(hash string) (*models.Torrent, error) {
	return uc.repo.GetTorrent(hash)
}

func (uc *TorrentUseCase) Pause(hash string) error {
	if err := uc.engine.Pause(hash); err != nil {
		return err
	}

	t, err := uc.repo.GetTorrent(hash)
	if err == nil && t != nil {
		t.State = models.StatePaused
		_ = uc.repo.SaveTorrent(t)
	}
	return nil
}

func (uc *TorrentUseCase) Resume(hash string) error {
	if err := uc.engine.Resume(hash); err != nil {
		return err
	}

	t, err := uc.repo.GetTorrent(hash)
	if err == nil && t != nil {
		t.State = models.StateQueued
		_ = uc.repo.SaveTorrent(t)
	}
	return nil
}

func (uc *TorrentUseCase) Delete(hash string, deleteFiles bool) error {
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

func (uc *TorrentUseCase) AddStream(ctx context.Context, hash string, index int) error {
	return uc.engine.StreamManager().AddStream(ctx, hash, index)
}

func (uc *TorrentUseCase) GetTorrentFile(hash string, index int) (*torrent.File, error) {
	return uc.engine.GetTorrentFile(hash, index)
}
