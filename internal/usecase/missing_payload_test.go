package usecase

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
)

func TestMissingExpectedPayloadPersistsAndRequiresExplicitRepairAfterReopen(t *testing.T) {
	uc, r, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor, path := completedSafetyFile(t, uc, "missing-expected", false)
	if err := uc.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	if ready, _, err := uc.engine.CheckFileDirectReady(tor.Hash, 0); err != nil || !ready {
		t.Fatal("fixture lost verification before disappearance", ready, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if src, err := uc.engine.OpenStreamSource(context.Background(), tor.Hash, 0, false); err == nil {
		src.Close()
		t.Fatal("missing payload opened")
	}
	i, err := r.GetIntent(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := r.ActivationBlocks(i.Version)
	if err != nil || len(blocks) != 1 || blocks[0].Kind != models.BlockMissingFiles {
		t.Fatal("physical disappearance not durable", blocks, err)
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
	if err := restored.RestoreTorrents(); err != nil {
		t.Fatal(err)
	}
	if raw := e.GetRawTorrent(tor.Hash); raw != nil && (raw.NetworkingEnabled() || raw.DataDownloadAllowed()) {
		t.Fatal("reopen repaired without authorization")
	}
	if err := restored.Resume(tor.Hash); err != nil {
		t.Fatal(err)
	}
	view, err := restored.GetTorrent(tor.Hash)
	if err != nil || len(view.Control.ActivationBlocks) != 0 || view.Control.DesiredRun != models.RunEnabled || view.State == models.StateSeeding || view.ProgressSource == "verified" && view.Downloaded > 0 {
		t.Fatal("repair invented readiness", view, err)
	}
}
