package usecase

import (
	"errors"
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

func TestPauseMissingTorrentReturnsNotFound(t *testing.T) {
	uc, _, cleanup := setupTorrentUseCase(t)
	defer cleanup()

	err := uc.Pause(testInfoHash)
	if !errors.Is(err, ErrTorrentNotFound) {
		t.Fatalf("expected not found error, got %v", err)
	}
}
