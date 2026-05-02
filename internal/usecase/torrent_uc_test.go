package usecase

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
)

const testInfoHash = "0123456789abcdef0123456789abcdef01234567"

func setupTorrentUseCase(t *testing.T) (*TorrentUseCase, *repository.TorrentRepo, func()) {
	t.Helper()

	dir := t.TempDir()
	db, err := repository.NewSQLiteDB(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatalf("failed to create test DB: %v", err)
	}

	eng, err := engine.New(&config.Config{
		DownloadDir:        dir,
		TorrentPort:        0,
		MaxActiveDownloads: 1,
		MinFreeSpaceGB:     0,
	})
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	repo := repository.NewTorrentRepo(db)
	cleanup := func() {
		eng.Close()
		db.Close()
	}

	return NewTorrentUseCase(eng, repo), repo, cleanup
}

func TestPauseDBOnlyTorrentUpdatesRepository(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()

	if err := repo.SaveTorrent(&models.Torrent{Hash: testInfoHash, Name: testInfoHash, State: models.StateQueued}); err != nil {
		t.Fatalf("failed to save torrent: %v", err)
	}

	if err := uc.Pause(testInfoHash); err != nil {
		t.Fatalf("pause failed: %v", err)
	}

	fetched, err := repo.GetTorrent(testInfoHash)
	if err != nil {
		t.Fatalf("failed to fetch torrent: %v", err)
	}
	if fetched.State != models.StatePaused {
		t.Fatalf("expected paused state, got %s", fetched.State)
	}
}

func TestPauseMagnetWithoutMetadataDoesNotPanic(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()

	if _, err := uc.engine.AddInfoHash(testInfoHash); err != nil {
		t.Fatalf("failed to add info hash to engine: %v", err)
	}
	if err := repo.SaveTorrent(&models.Torrent{Hash: testInfoHash, Name: testInfoHash, State: models.StateDownloading}); err != nil {
		t.Fatalf("failed to save torrent: %v", err)
	}

	if err := uc.Pause(testInfoHash); err != nil {
		t.Fatalf("pause failed: %v", err)
	}

	fetched, err := repo.GetTorrent(testInfoHash)
	if err != nil {
		t.Fatalf("failed to fetch torrent: %v", err)
	}
	if fetched.State != models.StatePaused {
		t.Fatalf("expected paused state, got %s", fetched.State)
	}
}

func TestResumeDBOnlyTorrentRestoresEngine(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()

	if err := repo.SaveTorrent(&models.Torrent{Hash: testInfoHash, Name: testInfoHash, State: models.StatePaused}); err != nil {
		t.Fatalf("failed to save torrent: %v", err)
	}

	if err := uc.Resume(testInfoHash); err != nil {
		t.Fatalf("resume failed: %v", err)
	}

	fetched, err := repo.GetTorrent(testInfoHash)
	if err != nil {
		t.Fatalf("failed to fetch torrent: %v", err)
	}
	if fetched.State != models.StateQueued {
		t.Fatalf("expected queued state in repository, got %s", fetched.State)
	}
	if fetched.SourceURI == "" {
		t.Fatal("expected resume to persist a fallback source URI")
	}

	engineTorrents := uc.engine.GetAllTorrents()
	if len(engineTorrents) != 1 {
		t.Fatalf("expected restored engine torrent, got %d", len(engineTorrents))
	}
	if engineTorrents[0].Hash != testInfoHash {
		t.Fatalf("expected hash %s, got %s", testInfoHash, engineTorrents[0].Hash)
	}
	if engineTorrents[0].State != models.StateDownloading {
		t.Fatalf("expected engine torrent to start downloading, got %s", engineTorrents[0].State)
	}
}

func TestRestoreTorrentsUsesPersistedSourceURI(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()

	source := "magnet:?xt=urn:btih:" + testInfoHash + "&tr=http%3A%2F%2Ftracker.example%2Fannounce"
	if err := repo.SaveTorrent(&models.Torrent{Hash: testInfoHash, Name: testInfoHash, State: models.StateQueued, SourceURI: source}); err != nil {
		t.Fatalf("failed to save torrent: %v", err)
	}

	if err := uc.RestoreTorrents(); err != nil {
		t.Fatalf("restore failed: %v", err)
	}

	fetched, err := repo.GetTorrent(testInfoHash)
	if err != nil {
		t.Fatalf("failed to fetch torrent: %v", err)
	}
	if fetched.SourceURI != source {
		t.Fatalf("expected source URI to be preserved, got %q", fetched.SourceURI)
	}
}

func TestRestoreTorrentsFallsBackFromInvalidMetainfoToSourceURI(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()

	source := "magnet:?xt=urn:btih:" + testInfoHash + "&tr=http%3A%2F%2Ftracker.example%2Fannounce"
	if err := repo.SaveTorrent(&models.Torrent{Hash: testInfoHash, Name: testInfoHash, State: models.StateQueued, SourceURI: source}); err != nil {
		t.Fatalf("failed to save torrent: %v", err)
	}
	metainfoPath := uc.engine.MetainfoPath(testInfoHash)
	if err := os.MkdirAll(filepath.Dir(metainfoPath), 0o755); err != nil {
		t.Fatalf("failed to create metainfo dir: %v", err)
	}
	if err := os.WriteFile(metainfoPath, []byte("not a torrent"), 0o600); err != nil {
		t.Fatalf("failed to write invalid metainfo: %v", err)
	}

	if err := uc.RestoreTorrents(); err != nil {
		t.Fatalf("restore should fall back to source URI: %v", err)
	}
	if _, err := os.Stat(metainfoPath + ".invalid"); err != nil {
		t.Fatalf("expected invalid metainfo to be renamed, stat err=%v", err)
	}

	engineTorrents := uc.engine.GetAllTorrents()
	if len(engineTorrents) != 1 {
		t.Fatalf("expected fallback to restore engine torrent, got %d", len(engineTorrents))
	}
	if engineTorrents[0].SourceURI != source {
		t.Fatalf("expected source URI to be preserved, got %q", engineTorrents[0].SourceURI)
	}
}

func TestPauseMissingTorrentReturnsNotFound(t *testing.T) {
	uc, _, cleanup := setupTorrentUseCase(t)
	defer cleanup()

	err := uc.Pause(testInfoHash)
	if !errors.Is(err, ErrTorrentNotFound) {
		t.Fatalf("expected not found error, got %v", err)
	}
}

func TestDeleteTorrentFilesRemovesSingleFileAndPartFile(t *testing.T) {
	uc, _, cleanup := setupTorrentUseCase(t)
	defer cleanup()

	downloadDir := uc.engine.DownloadDir()
	filePath := filepath.Join(downloadDir, "movie.mkv")
	partPath := filePath + ".part"
	if err := os.WriteFile(filePath, []byte("data"), 0o600); err != nil {
		t.Fatalf("failed to create file: %v", err)
	}
	if err := os.WriteFile(partPath, []byte("part"), 0o600); err != nil {
		t.Fatalf("failed to create part file: %v", err)
	}

	err := uc.deleteTorrentFiles(&models.Torrent{
		Name:  "movie.mkv",
		Files: []*models.File{{Path: "movie.mkv"}},
	})
	if err != nil {
		t.Fatalf("delete files failed: %v", err)
	}
	if _, err := os.Stat(filePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected file to be removed, stat err=%v", err)
	}
	if _, err := os.Stat(partPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected part file to be removed, stat err=%v", err)
	}
}

func TestDeleteTorrentFilesRemovesMultiFileStorageLayout(t *testing.T) {
	uc, _, cleanup := setupTorrentUseCase(t)
	defer cleanup()

	downloadDir := uc.engine.DownloadDir()
	filePath := filepath.Join(downloadDir, "Torrent Root", "Season 1", "episode.mkv")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	if err := os.WriteFile(filePath, []byte("data"), 0o600); err != nil {
		t.Fatalf("failed to create file: %v", err)
	}

	err := uc.deleteTorrentFiles(&models.Torrent{
		Name:  "Torrent Root",
		Files: []*models.File{{Path: "Season 1/episode.mkv"}},
	})
	if err != nil {
		t.Fatalf("delete files failed: %v", err)
	}
	if _, err := os.Stat(filePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected nested file to be removed, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(downloadDir, "Torrent Root")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected empty torrent root to be pruned, stat err=%v", err)
	}
}

func TestDeleteTorrentFilesRejectsUnsafePath(t *testing.T) {
	uc, _, cleanup := setupTorrentUseCase(t)
	defer cleanup()

	err := uc.deleteTorrentFiles(&models.Torrent{
		Name:  "unsafe",
		Files: []*models.File{{Path: "../outside.mkv"}},
	})
	if err == nil {
		t.Fatal("expected unsafe path error")
	}
}
