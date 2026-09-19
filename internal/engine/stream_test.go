package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/models"

	"github.com/anacrolix/torrent/metainfo"
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
	err = sm.AddStream(ctx, "dummyhash", 0, StreamOptions{})
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

func TestStreamManager_AddStreamSkipsHeadAndInvalidRange(t *testing.T) {
	e := &Engine{}
	sm := NewStreamManager(e)

	if err := sm.AddStream(context.Background(), "hash", 0, StreamOptions{IsHEAD: true}); err != nil {
		t.Fatalf("expected HEAD stream QoS to be skipped, got %v", err)
	}
	if err := sm.AddStream(context.Background(), "hash", 0, StreamOptions{SkipQoS: true}); err != nil {
		t.Fatalf("expected skipped stream QoS to return nil, got %v", err)
	}

	if got := sm.ActiveStreamsTotal(); got != 0 {
		t.Fatalf("expected no active streams for skipped QoS, got %d", got)
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

func TestStreamManager_AccountingOnly_ReferenceCountingAndNoOverlay(t *testing.T) {
	fileData := [][]byte{bytes.Repeat([]byte("A"), 4096)}
	e, mt, _ := setupEngineWithTorrent(t, 1024, "test_acct", []metainfo.FileInfo{
		{Length: 4096, Path: []string{"video.mkv"}},
	}, fileData)
	hash := mt.Hash

	// Add accounting-only stream session
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sm := e.streamManager
	err := sm.AddStream(ctx, hash, 0, StreamOptions{AccountingOnly: true})
	if err != nil {
		t.Fatalf("failed to add accounting-only stream: %v", err)
	}

	if sm.ActiveStreamsForTorrent(hash) != 1 {
		t.Fatalf("expected ActiveStreamsForTorrent to be 1, got %d", sm.ActiveStreamsForTorrent(hash))
	}
	if sm.ActiveStreamsTotal() != 1 {
		t.Fatalf("expected ActiveStreamsTotal to be 1, got %d", sm.ActiveStreamsTotal())
	}

	// Verify no piece ranges were applied
	sm.mu.Lock()
	state := sm.states[FileKey{Hash: hash, Index: 0}]
	if state == nil {
		sm.mu.Unlock()
		t.Fatal("expected stream state to exist")
	}
	if len(state.AppliedRanges) != 0 {
		sm.mu.Unlock()
		t.Fatalf("expected 0 applied ranges for accounting-only stream, got %d", len(state.AppliedRanges))
	}
	sm.mu.Unlock()

	// Cancel context to trigger removal and debounce
	cancel()
	time.Sleep(50 * time.Millisecond)

	// Debounce timer should be active, still counting as 1 active stream
	if sm.ActiveStreamsForTorrent(hash) != 1 {
		t.Fatalf("expected ActiveStreamsForTorrent to remain 1 during debounce, got %d", sm.ActiveStreamsForTorrent(hash))
	}

	sm.mu.Lock()
	state = sm.states[FileKey{Hash: hash, Index: 0}]
	if state == nil || state.DebounceTimer == nil {
		sm.mu.Unlock()
		t.Fatal("expected debounce timer to be set")
	}
	sm.mu.Unlock()
}

func TestStreamManager_MixedSessions_OverlayPreserved(t *testing.T) {
	// 10 pieces of 1024 bytes each, uncompleted so QoS overlay computes ranges
	fileData := [][]byte{bytes.Repeat([]byte("B"), 10240)}
	e, mt, _ := setupEngineWithTorrent(t, 1024, "test_mixed", []metainfo.FileInfo{
		{Length: 10240, Path: []string{"video.mp4"}},
	}, fileData)
	hash := mt.Hash

	ctxTorrent, cancelTorrent := context.WithCancel(context.Background())
	defer cancelTorrent()

	sm := e.streamManager

	// Session 1: torrent reader session with Range
	err := sm.AddStream(ctxTorrent, hash, 0, StreamOptions{
		AccountingOnly: false,
		RangeStart:     2048,
		HasRange:       true,
	})
	if err != nil {
		t.Fatalf("failed to add torrent stream: %v", err)
	}

	sm.mu.Lock()
	state := sm.states[FileKey{Hash: hash, Index: 0}]
	if state == nil || len(state.AppliedRanges) == 0 {
		sm.mu.Unlock()
		t.Fatal("expected applied ranges for torrent stream")
	}
	rangesBefore := cloneStreamPieceRanges(state.AppliedRanges)
	sm.mu.Unlock()

	// Session 2: accounting-only disk stream session
	ctxDisk, cancelDisk := context.WithCancel(context.Background())
	err = sm.AddStream(ctxDisk, hash, 0, StreamOptions{
		AccountingOnly: true,
	})
	if err != nil {
		cancelDisk()
		t.Fatalf("failed to add disk accounting stream: %v", err)
	}

	if sm.ActiveStreamsForTorrent(hash) != 2 {
		t.Fatalf("expected 2 active streams, got %d", sm.ActiveStreamsForTorrent(hash))
	}

	// Verify overlay ranges are unchanged
	sm.mu.Lock()
	state = sm.states[FileKey{Hash: hash, Index: 0}]
	if !reflect.DeepEqual(state.AppliedRanges, rangesBefore) {
		sm.mu.Unlock()
		t.Fatalf("expected applied ranges to be preserved, got %+v want %+v", state.AppliedRanges, rangesBefore)
	}
	sm.mu.Unlock()

	// Cancel disk session (accounting only)
	cancelDisk()
	time.Sleep(50 * time.Millisecond)

	// Should have 1 active stream left (ctxTorrent is still active)
	if sm.ActiveStreamsForTorrent(hash) != 1 {
		t.Fatalf("expected 1 active stream after disk session cancelled, got %d", sm.ActiveStreamsForTorrent(hash))
	}

	// Verify torrent overlay ranges are STILL preserved!
	sm.mu.Lock()
	state = sm.states[FileKey{Hash: hash, Index: 0}]
	if !reflect.DeepEqual(state.AppliedRanges, rangesBefore) {
		sm.mu.Unlock()
		t.Fatalf("expected applied ranges to remain intact after accounting session removed, got %+v want %+v", state.AppliedRanges, rangesBefore)
	}
	sm.mu.Unlock()
}

func TestStreamManager_DirectStream_PausedAndDiskFullStatePreserved(t *testing.T) {
	fileData := [][]byte{bytes.Repeat([]byte("Z"), 2048)}
	e, mt, dir := setupEngineWithTorrent(t, 1024, "test_paused_stream", []metainfo.FileInfo{
		{Length: 2048, Path: []string{"movie.mkv"}},
	}, fileData)
	hash := mt.Hash

	filePath := filepath.Join(dir, "test_paused_stream", "movie.mkv")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	if err := os.WriteFile(filePath, fileData[0], 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	verifyEngineTorrentPieces(t, e, hash, 0, 2)

	// Now pause the torrent
	if err := e.Pause(hash); err != nil {
		t.Fatalf("failed to pause torrent: %v", err)
	}
	if got := e.GetTorrent(hash).State; got != models.StatePaused {
		t.Fatalf("expected state Paused, got %s", got)
	}

	// Open direct stream source
	src, err := e.OpenStreamSource(context.Background(), hash, 0, false)
	if err != nil {
		t.Fatalf("failed to open stream source: %v", err)
	}
	if src.Type != StreamSourceDisk {
		src.Close()
		t.Fatalf("expected StreamSourceDisk, got %s", src.Type)
	}

	// Read some bytes
	buf := make([]byte, 100)
	n, err := src.Reader.Read(buf)
	if err != nil || n != 100 {
		src.Close()
		t.Fatalf("read failed: n=%d err=%v", n, err)
	}
	src.Close()

	// Verify state is STILL Paused
	if got := e.GetTorrent(hash).State; got != models.StatePaused {
		t.Fatalf("expected state to remain Paused after direct stream, got %s", got)
	}
}
