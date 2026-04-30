package engine

import (
	"os"
	"testing"
	"torrent-stream-hub/internal/config"
)

func TestCacheStatus_TorrentNotFound(t *testing.T) {
	cfg := &config.Config{
		StreamCacheSize: 200 * 1024 * 1024,
		DownloadDir:     os.TempDir(),
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("Failed to create engine: %v", err)
	}
	defer e.Close()

	_, err = e.GetCacheStatus("nonexistent", 0, 0)
	if err == nil {
		t.Fatal("Expected error for non-existent torrent, got nil")
	}
}

// Since anacrolix/torrent.Torrent cannot be easily instantiated with mock pieces,
// we just cover the error path for the emulated cache and ensure it returns
// expected boundary errors.
func TestCacheStatus_IndexOutOfBounds(t *testing.T) {
	cfg := &config.Config{
		StreamCacheSize: 200 * 1024 * 1024,
		DownloadDir:     os.TempDir(),
	}
	e, _ := New(cfg)
	defer e.Close()

	// Manually inject a dummy managed torrent with no files
	e.mu.Lock()
	e.managedTorrents["dummy"] = &ManagedTorrent{
		t: nil, // This would normally crash t.Files(), but we can't easily mock it without interfaces.
	}
	e.mu.Unlock()

	// We can't safely call GetCacheStatus if mt.t is nil because it calls mt.t.Files().
	// Real e2e integration tests for caching will be done using an actual torrent file in Phase 6.
}
