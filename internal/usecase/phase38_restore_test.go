package usecase

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
)

func TestPausedVerifiedReadAfterRuntimeReopen(t *testing.T) {
	uc, _, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor, _ := completedSafetyFile(t, uc, "paused-reopen", true)
	if err := uc.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	cleanup()
	e, err := engine.New(&config.Config{DownloadDir: uc.engine.DownloadDir(), DBPath: filepath.Join(uc.engine.DownloadDir(), "hub.db"), BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true})
	if err != nil {
		t.Fatal(err)
	}
	db, err := repository.NewSQLiteDB(filepath.Join(uc.engine.DownloadDir(), "hub.db"))
	if err != nil {
		e.Close()
		t.Fatal(err)
	}
	defer func() { e.Close(); db.Close() }()
	restored := NewTorrentUseCase(e, repository.NewTorrentRepo(db))
	if err := restored.RestoreTorrents(); err != nil {
		t.Fatal(err)
	}
	raw := e.GetRawTorrent(tor.Hash)
	if raw == nil || raw.DataDownloadAllowed() || raw.NetworkingEnabled() {
		t.Fatal("paused restore enabled networking")
	}
	source, err := restored.OpenStreamSource(context.Background(), tor.Hash, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	data, err := io.ReadAll(source.Reader)
	if err != nil || len(data) != 1024 {
		t.Fatal("paused direct read failed", len(data), err)
	}
	if raw.DataDownloadAllowed() || raw.NetworkingEnabled() || e.GetTorrent(tor.Hash).State != models.StatePaused {
		t.Fatal("reading implicitly resumed torrent")
	}
	_, file, err := restored.GetDownloadedFile(tor.Hash, 0)
	if err != nil {
		t.Fatal(err)
	}
	f, err := file.Open()
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestQueueOrderSurvivesRestoreAndResume(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	hashes := []string{"1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222", "3333333333333333333333333333333333333333"}
	var positions []int64
	for _, hash := range hashes {
		if _, err := uc.AddMagnet("magnet:?xt=urn:btih:" + hash); err != nil {
			t.Fatal(err)
		}
		saved, err := repo.GetTorrent(hash)
		if err != nil {
			t.Fatal(err)
		}
		positions = append(positions, saved.QueuePosition)
	}
	cleanup()
	e, err := engine.New(&config.Config{DownloadDir: uc.engine.DownloadDir(), DBPath: filepath.Join(uc.engine.DownloadDir(), "hub.db"), MaxActiveDownloads: 1, BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true})
	if err != nil {
		t.Fatal(err)
	}
	db, err := repository.NewSQLiteDB(filepath.Join(uc.engine.DownloadDir(), "hub.db"))
	if err != nil {
		e.Close()
		t.Fatal(err)
	}
	defer func() { e.Close(); db.Close() }()
	repo = repository.NewTorrentRepo(db)
	restored := NewTorrentUseCase(e, repo)
	if err := restored.RestoreTorrents(); err != nil {
		t.Fatal(err)
	}
	if e.GetTorrent(hashes[0]).State != models.StateDownloading || e.GetTorrent(hashes[1]).State != models.StateQueued {
		t.Fatal("restore changed FIFO")
	}
	if err := restored.Pause(hashes[0]); err != nil {
		t.Fatal(err)
	}
	if err := restored.Resume(hashes[2]); err != nil {
		t.Fatal(err)
	}
	if e.GetTorrent(hashes[1]).State != models.StateDownloading {
		t.Fatal("Resume jumped ahead of older queued torrent")
	}
	for i, hash := range hashes {
		saved, err := repo.GetTorrent(hash)
		if err != nil || saved.QueuePosition != positions[i] {
			t.Fatal("command changed queue sequence", err)
		}
	}
}
