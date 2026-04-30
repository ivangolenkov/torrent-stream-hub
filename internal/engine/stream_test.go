package engine

import (
	"context"
	"os"
	"testing"
	"time"
	"torrent-stream-hub/internal/config"
)

func TestStreamManager_AddRemoveStream_NotFound(t *testing.T) {
	cfg := &config.Config{
		DownloadDir: os.TempDir(),
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("Failed to create engine: %v", err)
	}
	defer e.Close()

	sm := NewStreamManager(e)
	ctx := context.Background()

	// Should fail because torrent is not in engine
	err = sm.AddStream(ctx, "dummyhash", 0)
	if err == nil {
		t.Fatal("Expected error for non-existent torrent, got nil")
	}

	sm.mu.Lock()
	state, exists := sm.states[FileKey{Hash: "dummyhash", Index: 0}]
	sm.mu.Unlock()

	if exists && state.ActiveStreams != 0 {
		t.Errorf("Expected active streams to be 0 after failure, got %d", state.ActiveStreams)
	}
}

func TestStreamManager_ContextCancellation(t *testing.T) {
	cfg := &config.Config{
		DownloadDir: os.TempDir(),
	}
	e, _ := New(cfg)
	defer e.Close()
	sm := NewStreamManager(e)

	// Since we can't easily mock anacrolix/torrent.Torrent,
	// we'll just test that context cancellation triggers RemoveStream
	// by manually inserting a dummy state and checking if it gets cleaned up.

	key := FileKey{Hash: "testhash", Index: 1}
	sm.mu.Lock()
	sm.states[key] = &StreamState{
		ActiveStreams: 1, // pretend it's active
	}
	sm.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())

	// Start a goroutine that waits for ctx.Done() and calls RemoveStream
	go func() {
		<-ctx.Done()
		sm.RemoveStream("testhash", 1)
	}()

	// Cancel context
	cancel()

	// Wait a bit for goroutine
	time.Sleep(50 * time.Millisecond)

	sm.mu.Lock()
	state, exists := sm.states[key]
	sm.mu.Unlock()

	if !exists {
		t.Fatal("State should exist but have debounce timer")
	}

	if state.ActiveStreams != 0 {
		t.Errorf("Expected ActiveStreams 0, got %d", state.ActiveStreams)
	}
	if state.DebounceTimer == nil {
		t.Error("Expected DebounceTimer to be set")
	}
}
