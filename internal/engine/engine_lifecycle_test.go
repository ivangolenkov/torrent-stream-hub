package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/models"

	"github.com/anacrolix/torrent/metainfo"
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

func TestEngineCloseStopsBackgroundMonitors(t *testing.T) {
	tests := []struct {
		name            string
		swarmWatchdogOn bool
	}{
		{name: "with swarm watchdog", swarmWatchdogOn: true},
		{name: "without swarm watchdog", swarmWatchdogOn: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := &config.Config{
				DownloadDir:                dir,
				DBPath:                     filepath.Join(dir, "hub.db"),
				TorrentPort:                0,
				BTDisableDHT:               true,
				BTDisablePEX:               true,
				BTDisableUPNP:              true,
				BTDisableUTP:               true,
				BTNoUpload:                 true,
				BTSwarmWatchdogEnabled:     tt.swarmWatchdogOn,
				BTSwarmWatchdogConfigured:  true,
				BTSwarmCheckIntervalSec:    1,
				BTSwarmRefreshCooldownSec:  1,
				BTSwarmStalledDurationSec:  1,
				BTClientRecycleMaxPerHour:  1,
				BTClientRecycleMinTorrents: 1,
			}

			e, err := New(cfg)
			if err != nil {
				t.Fatalf("failed to create engine: %v", err)
			}

			done := make(chan struct{})
			go func() {
				e.Close()
				e.Close()
				close(done)
			}()

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatalf("engine close timed out")
			}
		})
	}
}

func TestEngineLifecycle_DirectStreamClosedOnTorrentDelete(t *testing.T) {
	fileData := [][]byte{bytes.Repeat([]byte("X"), 2048)}
	e, mt, dir := setupEngineWithTorrent(t, 1024, "test_del_stream", []metainfo.FileInfo{
		{Length: 2048, Path: []string{"video.mp4"}},
	}, fileData)
	hash := mt.Hash

	filePath := filepath.Join(dir, "test_del_stream", "video.mp4")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	if err := os.WriteFile(filePath, fileData[0], 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	verifyEngineTorrentPieces(t, e, hash, 0, 2)

	src, err := e.OpenStreamSource(context.Background(), hash, 0, false)
	if err != nil {
		t.Fatalf("failed to open direct stream source: %v", err)
	}
	defer src.Close()

	if src.Type != StreamSourceDisk {
		t.Fatalf("expected StreamSourceDisk, got %s", src.Type)
	}

	// Delete torrent while stream is open
	err = e.Delete(hash)
	if err != nil {
		t.Fatalf("failed to delete torrent: %v", err)
	}

	// Read on deleted torrent's stream should fail
	buf := make([]byte, 100)
	_, err = src.Reader.Read(buf)
	if err == nil {
		t.Fatal("expected read to fail after torrent deletion, got nil error")
	}
}

func TestEngineLifecycle_DirectStreamClosedOnEngineClose(t *testing.T) {
	fileData := [][]byte{bytes.Repeat([]byte("Y"), 2048)}
	e, mt, dir := setupEngineWithTorrent(t, 1024, "test_close_stream", []metainfo.FileInfo{
		{Length: 2048, Path: []string{"video.mp4"}},
	}, fileData)
	hash := mt.Hash

	filePath := filepath.Join(dir, "test_close_stream", "video.mp4")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	if err := os.WriteFile(filePath, fileData[0], 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	verifyEngineTorrentPieces(t, e, hash, 0, 2)

	src, err := e.OpenStreamSource(context.Background(), hash, 0, false)
	if err != nil {
		t.Fatalf("failed to open direct stream source: %v", err)
	}
	defer src.Close()

	if src.Type != StreamSourceDisk {
		t.Fatalf("expected StreamSourceDisk, got %s", src.Type)
	}

	// Close engine while stream is open
	e.Close()

	// Read on closed engine's stream should fail
	buf := make([]byte, 100)
	_, err = src.Reader.Read(buf)
	if err == nil {
		t.Fatal("expected read to fail after engine close, got nil error")
	}
}

func TestEngineLifecycle_RecheckPolicyOnDirectStream(t *testing.T) {
	fileData := [][]byte{bytes.Repeat([]byte("R"), 2048)}
	e, mt, dir := setupEngineWithTorrent(t, 1024, "test_recheck_stream", []metainfo.FileInfo{
		{Length: 2048, Path: []string{"video.mp4"}},
	}, fileData)
	hash := mt.Hash

	filePath := filepath.Join(dir, "test_recheck_stream", "video.mp4")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	if err := os.WriteFile(filePath, fileData[0], 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	verifyEngineTorrentPieces(t, e, hash, 0, 2)

	// Open stream 1 (disk source)
	src1, err := e.OpenStreamSource(context.Background(), hash, 0, false)
	if err != nil {
		t.Fatalf("failed to open direct stream source 1: %v", err)
	}
	defer src1.Close()

	if src1.Type != StreamSourceDisk {
		t.Fatalf("expected StreamSourceDisk for src1, got %s", src1.Type)
	}

	// External truncation/corruption of file
	_ = os.Chmod(filePath, 0o666)
	if err := os.Truncate(filePath, 1000); err != nil {
		t.Fatalf("failed to truncate file: %v", err)
	}

	// Existing open stream 1 can still read from its opened descriptor
	buf := make([]byte, 100)
	n, err := src1.Reader.Read(buf)
	if err != nil || n != 100 {
		t.Fatalf("expected src1 to read from existing descriptor, n=%d err=%v", n, err)
	}

	// New stream request will check size/readiness and fall back to torrent reader
	src2, err := e.OpenStreamSource(context.Background(), hash, 0, false)
	if err != nil {
		t.Fatalf("failed to open stream source 2: %v", err)
	}
	defer src2.Close()

	if src2.Type != StreamSourceTorrent {
		t.Fatalf("expected StreamSourceTorrent for new request after file change, got %s", src2.Type)
	}
	if src2.FallbackReason != FallbackReasonSizeMismatch {
		t.Fatalf("expected FallbackReasonSizeMismatch, got %s", src2.FallbackReason)
	}
}
