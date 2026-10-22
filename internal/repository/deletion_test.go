package repository

import (
	"path/filepath"
	"testing"

	"torrent-stream-hub/internal/models"
)

func TestDeletionUpgradeImportsExistingManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.sqlite")
	db, err := NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	repo := NewTorrentRepo(db)
	if err := repo.SaveTorrent(&models.Torrent{Hash: "old", State: models.StateDownloading}); err != nil {
		t.Fatal(err)
	}
	if err := repo.BeginCleanup("old", []string{"file"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`ALTER TABLE torrents DROP COLUMN deletion_mode; ALTER TABLE torrents DROP COLUMN deletion_error; DELETE FROM migrations WHERE name='0007_deletion_intent.sql'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo = NewTorrentRepo(db)
	saved, err := repo.GetTorrent("old")
	if err != nil {
		t.Fatal(err)
	}
	if saved.DeletionMode != "payload" {
		t.Fatal("upgrade lost legacy pending cleanup")
	}
	paths, pending, err := repo.CleanupPaths("old")
	if err != nil || !pending || len(paths) != 1 || paths[0] != "file" {
		t.Fatal("upgrade changed manifest", err)
	}
	ready, err := repo.CleanupReady("old")
	if err != nil || ready {
		t.Fatal("incomplete manifest became ready", err)
	}
	if err := repo.BeginForget("old"); err == nil {
		t.Fatal("mode changed silently")
	}
}
