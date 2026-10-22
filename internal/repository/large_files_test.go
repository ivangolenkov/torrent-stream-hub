package repository

import (
	"fmt"
	"testing"

	"torrent-stream-hub/internal/models"
)

func TestLargeFileSnapshotAndRollback(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewTorrentRepo(db)
	tor := &models.Torrent{Hash: "large", Name: "large", State: models.StatePaused}
	// 6,000 * 7 parameters exceeded SQLite's former single-statement bind limit.
	for i := range 6000 {
		tor.Files = append(tor.Files, &models.File{Index: i, Path: fmt.Sprint(i), Size: 1, Priority: models.PriorityHigh})
	}
	if err := repo.SaveTorrent(tor); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateFilePriority(tor.Hash, 0, models.PriorityNone); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveTorrent(tor); err != nil {
		t.Fatal(err)
	}
	saved, err := repo.GetTorrent(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Files) != 6000 || saved.Files[0].Priority != models.PriorityNone {
		t.Fatal("files or user priorities lost")
	}
	if _, err := db.DB().Exec(`CREATE TRIGGER fail_file BEFORE INSERT ON files WHEN NEW."index"=3000 BEGIN SELECT RAISE(ABORT,'file failure'); END`); err != nil {
		t.Fatal(err)
	}
	tor.Hash = "rollback"
	if err := repo.SaveTorrent(tor); err == nil {
		t.Fatal("ignored mid-transaction failure")
	}
	if saved, err := repo.GetTorrent(tor.Hash); err != nil || saved != nil {
		t.Fatal("partial torrent committed", err)
	}
	var count int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM files WHERE hash='rollback'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial files committed", err)
	}
}

func TestInvalidFileSnapshotIsRejectedBeforeWrite(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	repo := NewTorrentRepo(db)
	for _, files := range [][]*models.File{{nil}, {{Index: -1}}, {{Size: -1}}, {{Priority: 42}}, {{Index: 0}, {Index: 0}}} {
		if err := repo.SaveTorrent(&models.Torrent{Hash: "invalid", Files: files}); err == nil {
			t.Fatal("invalid file accepted")
		}
	}
	if saved, err := repo.GetTorrent("invalid"); err != nil || saved != nil {
		t.Fatal("invalid snapshot committed", err)
	}
}
