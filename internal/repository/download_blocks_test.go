package repository

import (
	"path/filepath"
	"testing"

	"torrent-stream-hub/internal/models"
)

func TestSnapshotsPreservePrioritiesAndDownloadLatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	db, err := NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	repo := NewTorrentRepo(db)
	m := &models.Torrent{Hash: "test", State: models.StatePaused, Files: []*models.File{{Index: 0, Path: "a", Priority: models.PriorityNone}}}
	if err := repo.SaveTorrent(m); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveDownloadBlock(m.Hash, "disk space unknown"); err != nil {
		t.Fatal(err)
	}
	m.Files[0].Priority = models.PriorityNormal
	if err := repo.SaveTorrent(m); err != nil {
		t.Fatal(err)
	}
	m.Files = nil
	if err := repo.SaveTorrent(m); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo = NewTorrentRepo(db)
	saved, err := repo.GetTorrent(m.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Files[0].Priority != models.PriorityNone || saved.DownloadBlock != "disk space unknown" {
		t.Fatalf("intent overwritten: %+v", saved)
	}
	if err := repo.SaveDownloadBlock(m.Hash, ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteTorrent(m.Hash); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradePreservesExistingDiskFull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.sqlite")
	db, err := NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewTorrentRepo(db).SaveTorrent(&models.Torrent{Hash: "old", State: models.StateDiskFull, Error: models.ErrDiskFull}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`DROP TABLE torrent_download_blocks; DELETE FROM migrations WHERE name='0006_download_blocks.sql'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	saved, err := NewTorrentRepo(db).GetTorrent("old")
	if err != nil {
		t.Fatal(err)
	}
	if saved.DownloadBlock == "" || saved.State != models.StateDiskFull {
		t.Fatal("upgrade erased existing disk latch")
	}
}
