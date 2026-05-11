package repository

import (
	"os"
	"path/filepath"
	"testing"
	"torrent-stream-hub/internal/models"
)

func setupTestDB(t *testing.T) (*SQLiteDB, func()) {
	t.Helper()

	// Create a temporary directory for the DB file
	dir, err := os.MkdirTemp("", "torrent-hub-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	dbPath := filepath.Join(dir, "test.db")

	db, err := NewSQLiteDB(dbPath)
	if err != nil {
		t.Fatalf("Failed to initialize test DB: %v", err)
	}

	cleanup := func() {
		db.Close()
		os.RemoveAll(dir)
	}

	return db, cleanup
}

func TestSaveAndGetTorrent(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewTorrentRepo(db)

	// Create a test torrent
	torrent := &models.Torrent{
		Hash:       "dummyhash123",
		Name:       "Test Torrent",
		Size:       1024,
		Downloaded: 512,
		State:      models.StateDownloading,
		Error:      models.ErrNone,
		SourceURI:  "magnet:?xt=urn:btih:dummyhash123",
		Title:      "Custom Title",
		Data:       `{"kinopoisk":"1"}`,
		Poster:     "https://example.com/poster.jpg",
		Category:   "movies",
		Files: []*models.File{
			{Index: 0, Path: "file1.txt", Size: 512, Downloaded: 256, Priority: models.PriorityNormal, IsMedia: false},
			{Index: 1, Path: "video.mp4", Size: 512, Downloaded: 256, Priority: models.PriorityHigh, IsMedia: true},
		},
	}

	// 1. Save
	if err := repo.SaveTorrent(torrent); err != nil {
		t.Fatalf("Failed to save torrent: %v", err)
	}

	// 2. Get
	fetched, err := repo.GetTorrent("dummyhash123")
	if err != nil {
		t.Fatalf("Failed to get torrent: %v", err)
	}

	if fetched == nil {
		t.Fatalf("Expected torrent to be found, got nil")
	}

	// Verify fields
	if fetched.Hash != torrent.Hash || fetched.Name != torrent.Name || fetched.Size != torrent.Size || fetched.SourceURI != torrent.SourceURI {
		t.Errorf("Metadata mismatch: expected %+v, got %+v", torrent, fetched)
	}
	if fetched.Title != torrent.Title || fetched.Data != torrent.Data || fetched.Poster != torrent.Poster || fetched.Category != torrent.Category {
		t.Errorf("TorrServer metadata mismatch: expected %+v, got %+v", torrent, fetched)
	}
	if fetched.State != torrent.State {
		t.Errorf("Expected state %s, got %s", torrent.State, fetched.State)
	}

	// Verify files
	if len(fetched.Files) != 2 {
		t.Fatalf("Expected 2 files, got %d", len(fetched.Files))
	}

	f1 := fetched.Files[0]
	if f1.Path != "file1.txt" || f1.Size != 512 || f1.Priority != models.PriorityNormal || f1.IsMedia != false {
		t.Errorf("File 1 metadata mismatch: %+v", f1)
	}

	f2 := fetched.Files[1]
	if f2.Path != "video.mp4" || f2.Priority != models.PriorityHigh || f2.IsMedia != true {
		t.Errorf("File 2 metadata mismatch: %+v", f2)
	}

	// 3. Update existing
	torrent.State = models.StateSeeding
	torrent.Downloaded = 1024
	torrent.SourceURI = ""
	torrent.Title = ""
	torrent.Data = ""
	torrent.Poster = ""
	torrent.Category = ""
	torrent.Files[0].Downloaded = 512
	torrent.Files[1].Downloaded = 512

	if err := repo.SaveTorrent(torrent); err != nil {
		t.Fatalf("Failed to update torrent: %v", err)
	}

	fetchedUpdated, _ := repo.GetTorrent("dummyhash123")
	if fetchedUpdated.State != models.StateSeeding || fetchedUpdated.Downloaded != 1024 {
		t.Errorf("Update failed. State: %s, Downloaded: %d", fetchedUpdated.State, fetchedUpdated.Downloaded)
	}
	if fetchedUpdated.SourceURI != "magnet:?xt=urn:btih:dummyhash123" {
		t.Errorf("Expected source URI to be preserved, got %q", fetchedUpdated.SourceURI)
	}
	if fetchedUpdated.Title != "Custom Title" || fetchedUpdated.Data != `{"kinopoisk":"1"}` || fetchedUpdated.Poster != "https://example.com/poster.jpg" || fetchedUpdated.Category != "movies" {
		t.Errorf("Expected TorrServer metadata to be preserved, got %+v", fetchedUpdated)
	}
	if fetchedUpdated.Files[0].Downloaded != 512 {
		t.Errorf("File update failed. Downloaded: %d", fetchedUpdated.Files[0].Downloaded)
	}

	// 4. GetAll
	all, err := repo.GetAllTorrents()
	if err != nil {
		t.Fatalf("GetAllTorrents failed: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("Expected 1 torrent in GetAll, got %d", len(all))
	}
	if len(all[0].Files) != 2 {
		t.Fatalf("Expected GetAll to include 2 files, got %d", len(all[0].Files))
	}

	// 5. Delete
	if err := repo.DeleteTorrent("dummyhash123"); err != nil {
		t.Fatalf("Failed to delete: %v", err)
	}

	deleted, _ := repo.GetTorrent("dummyhash123")
	if deleted != nil {
		t.Fatalf("Expected torrent to be nil after deletion")
	}
}

func TestSaveTorrentDoesNotDecreaseProgress(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewTorrentRepo(db)
	if err := repo.SaveTorrent(&models.Torrent{
		Hash:       "progresshash",
		Name:       "Progress Torrent",
		Size:       1000,
		Downloaded: 700,
		State:      models.StateDownloading,
		Files: []*models.File{
			{Index: 0, Path: "a.mkv", Size: 1000, Downloaded: 700, Priority: models.PriorityNormal, IsMedia: true},
		},
	}); err != nil {
		t.Fatalf("failed to save initial torrent: %v", err)
	}

	if err := repo.SaveTorrent(&models.Torrent{
		Hash:       "progresshash",
		Name:       "Progress Torrent",
		Size:       1000,
		Downloaded: 0,
		State:      models.StateDownloading,
		Files: []*models.File{
			{Index: 0, Path: "a.mkv", Size: 1000, Downloaded: 0, Priority: models.PriorityNormal, IsMedia: true},
		},
	}); err != nil {
		t.Fatalf("failed to save lower progress: %v", err)
	}

	fetched, err := repo.GetTorrent("progresshash")
	if err != nil {
		t.Fatalf("failed to fetch torrent: %v", err)
	}
	if fetched.Downloaded != 700 {
		t.Fatalf("expected torrent progress to stay 700, got %d", fetched.Downloaded)
	}
	if len(fetched.Files) != 1 || fetched.Files[0].Downloaded != 700 {
		t.Fatalf("expected file progress to stay 700, got %+v", fetched.Files)
	}
}

func TestGetNonExistentTorrent(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewTorrentRepo(db)

	fetched, err := repo.GetTorrent("not-exist")
	if err != nil {
		t.Fatalf("Expected nil error for non-existent torrent, got %v", err)
	}
	if fetched != nil {
		t.Fatalf("Expected nil result, got %+v", fetched)
	}
}

func TestGetAllTorrentsBatchLoadsFilesAndPreservesOrder(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	repo := NewTorrentRepo(db)
	torrents := []*models.Torrent{
		{
			Hash:       "oldhash",
			Name:       "Old Torrent",
			Size:       300,
			Downloaded: 120,
			State:      models.StateDownloading,
			Files: []*models.File{
				{Index: 2, Path: "z.mkv", Size: 100, Downloaded: 20, Priority: models.PriorityHigh, IsMedia: true},
				{Index: 0, Path: "a.txt", Size: 200, Downloaded: 100, Priority: models.PriorityNormal, IsMedia: false},
			},
		},
		{
			Hash:       "emptyhash",
			Name:       "No Files Torrent",
			Size:       0,
			Downloaded: 0,
			State:      models.StatePaused,
		},
		{
			Hash:       "newhash",
			Name:       "New Torrent",
			Size:       100,
			Downloaded: 100,
			State:      models.StateSeeding,
			Files: []*models.File{
				{Index: 1, Path: "b.mp4", Size: 100, Downloaded: 100, Priority: models.PriorityNone, IsMedia: true},
			},
		},
	}

	for _, torrent := range torrents {
		if err := repo.SaveTorrent(torrent); err != nil {
			t.Fatalf("failed to save torrent %s: %v", torrent.Hash, err)
		}
	}

	createdAt := map[string]string{
		"oldhash":   "2026-01-01 00:00:00",
		"newhash":   "2026-01-02 00:00:00",
		"emptyhash": "2026-01-03 00:00:00",
	}
	for hash, value := range createdAt {
		if _, err := db.DB().Exec(`UPDATE torrents SET created_at = ? WHERE hash = ?`, value, hash); err != nil {
			t.Fatalf("failed to set created_at for %s: %v", hash, err)
		}
	}

	all, err := repo.GetAllTorrents()
	if err != nil {
		t.Fatalf("GetAllTorrents failed: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 torrents, got %d", len(all))
	}

	wantOrder := []string{"emptyhash", "newhash", "oldhash"}
	for i, wantHash := range wantOrder {
		if all[i].Hash != wantHash {
			t.Fatalf("unexpected torrent order at %d: want %s, got %s", i, wantHash, all[i].Hash)
		}
	}

	if len(all[0].Files) != 0 {
		t.Fatalf("expected empty torrent to have no files, got %+v", all[0].Files)
	}
	if len(all[1].Files) != 1 || all[1].Files[0].Path != "b.mp4" || all[1].Files[0].Priority != models.PriorityNone || !all[1].Files[0].IsMedia {
		t.Fatalf("new torrent files mismatch: %+v", all[1].Files)
	}
	if len(all[2].Files) != 2 {
		t.Fatalf("expected old torrent to have 2 files, got %+v", all[2].Files)
	}
	if all[2].Files[0].Index != 0 || all[2].Files[0].Path != "a.txt" || all[2].Files[0].Downloaded != 100 {
		t.Fatalf("old torrent first file mismatch: %+v", all[2].Files[0])
	}
	if all[2].Files[1].Index != 2 || all[2].Files[1].Path != "z.mkv" || all[2].Files[1].Priority != models.PriorityHigh || !all[2].Files[1].IsMedia {
		t.Fatalf("old torrent second file mismatch: %+v", all[2].Files[1])
	}
}
