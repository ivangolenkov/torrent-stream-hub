package engine

import (
	"os"
	"testing"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/models"
)

func TestEngineLifecycle(t *testing.T) {
	cfg := &config.Config{
		DownloadDir:  os.TempDir(),
		DBPath:       ":memory:",
		BTDisableDHT: true,
		BTDisablePEX: true,
		BTNoUpload:   true,
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("Failed to create engine: %v", err)
	}
	defer e.Close()

	hash := "1111111111111111111111111111111111111111"
	magnet := "magnet:?xt=urn:btih:" + hash

	// Add
	t.Log("Adding magnet")
	mt, err := e.AddMagnet(magnet)
	if err != nil {
		t.Fatalf("AddMagnet failed: %v", err)
	}
	if mt.State != models.StateQueued && mt.State != models.StateDownloading {
		t.Fatalf("Expected Queued or Downloading, got %s", mt.State)
	}

	// Verify it's in the engine
	t.Log("Verifying it's in the engine")
	if e.GetTorrent(hash) == nil {
		t.Fatalf("Expected torrent to be in engine")
	}

	// Pause
	t.Log("Pausing torrent")
	err = e.Pause(hash)
	if err != nil {
		t.Fatalf("Pause failed: %v", err)
	}
	t.Log("Verifying paused state")
	if tModel := e.GetTorrent(hash); tModel == nil || tModel.State != models.StatePaused {
		t.Fatalf("Expected Paused state, got %v", tModel.State)
	}

	// Resume
	t.Log("Resuming torrent")
	err = e.Resume(hash)
	if err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	t.Log("Verifying resumed state")
	if tModel := e.GetTorrent(hash); tModel == nil || (tModel.State != models.StateQueued && tModel.State != models.StateDownloading) {
		t.Fatalf("Expected Queued/Downloading state, got %v", tModel.State)
	}

	// Delete
	t.Log("Deleting torrent")
	err = e.Delete(hash)
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	t.Log("Verifying deleted")
}
