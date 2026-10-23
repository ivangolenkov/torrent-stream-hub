package usecase

import (
	"os"
	"path/filepath"
	"testing"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
)

func TestMetadataApplyFaultIsDurableBeforeFailedCommandReturns(t *testing.T) {
	uc, r, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor := temporaryFile(t, uc, "metadata-apply-fault", false)
	if err := uc.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(uc.engine.ConfigDir(), "metainfo", tor.Hash+".torrent")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := uc.Resume(tor.Hash); err == nil {
		t.Fatal("reported success after metadata persistence failure")
	}
	i, err := r.GetIntent(tor.Hash)
	if err != nil || i.Run != models.RunEnabled {
		t.Fatal("apply failure rolled back commit", i, err)
	}
	blocks, err := r.ActivationBlocks(i.Version)
	if err != nil || len(blocks) != 1 || blocks[0].Kind != models.BlockMetadataPersistence {
		t.Fatal("returned before durable fault", blocks, err)
	}
	if raw := uc.engine.GetRawTorrent(tor.Hash); raw.DataDownloadAllowed() {
		t.Fatal("fault admitted payload")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	dir := uc.engine.DownloadDir()
	cleanup()
	db, err := repository.NewSQLiteDB(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e, err := engine.New(&config.Config{DownloadDir: dir, DBPath: filepath.Join(dir, "hub.db"), BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	restored := NewTorrentUseCase(e, repository.NewTorrentRepo(db))
	if view, err := restored.GetTorrent(tor.Hash); err != nil || view.State != models.StateError || view.Error != models.ErrMetadataPersistence {
		t.Fatal(view, err)
	}
	if err := restored.RestoreTorrents(); err != nil {
		t.Fatal(err)
	}
	if raw := e.GetRawTorrent(tor.Hash); raw.NetworkingEnabled() || raw.DataDownloadAllowed() {
		t.Fatal("fault automatically resumed after reopen")
	}
	if err := restored.Resume(tor.Hash); err != nil {
		t.Fatal(err)
	}
	if view, err := restored.GetTorrent(tor.Hash); err != nil || len(view.Control.ActivationBlocks) != 0 || view.Control.ReconcilePending {
		t.Fatal("explicit repaired retry failed", view, err)
	}
}
