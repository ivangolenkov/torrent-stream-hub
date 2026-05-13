package engine

import (
	"errors"
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

	_, err := e.GetCacheStatus("dummy", 0, 0)
	if err == nil {
		t.Fatal("expected metadata error")
	}
}

func TestContinuousCompleteBytesFromOffset(t *testing.T) {
	completePieces := map[int64]bool{0: true, 1: true, 2: false, 3: true}
	complete := func(pieceIndex int64) bool {
		return completePieces[pieceIndex]
	}

	tests := []struct {
		name          string
		fileStart     int64
		fileLength    int64
		currentOffset int64
		pieceLength   int64
		complete      func(int64) bool
		want          int64
	}{
		{
			name:          "negative offset clamps to start",
			fileStart:     512,
			fileLength:    3000,
			currentOffset: -100,
			pieceLength:   1024,
			complete:      complete,
			want:          1536,
		},
		{
			name:          "offset inside completed piece counts exact remaining bytes",
			fileStart:     512,
			fileLength:    3000,
			currentOffset: 600,
			pieceLength:   1024,
			complete:      complete,
			want:          936,
		},
		{
			name:          "incomplete first piece returns zero",
			fileStart:     0,
			fileLength:    3000,
			currentOffset: 2100,
			pieceLength:   1024,
			complete:      complete,
			want:          0,
		},
		{
			name:          "offset beyond eof returns zero",
			fileStart:     0,
			fileLength:    2500,
			currentOffset: 2500,
			pieceLength:   1024,
			complete:      complete,
			want:          0,
		},
		{
			name:          "last partial piece does not exceed file length",
			fileStart:     0,
			fileLength:    2500,
			currentOffset: 0,
			pieceLength:   1024,
			complete: func(pieceIndex int64) bool {
				return true
			},
			want: 2500,
		},
		{
			name:          "nil complete function returns zero",
			fileStart:     0,
			fileLength:    2500,
			currentOffset: 0,
			pieceLength:   1024,
			complete:      nil,
			want:          0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := continuousCompleteBytesFromOffset(tt.fileStart, tt.fileLength, tt.currentOffset, tt.pieceLength, tt.complete)
			if got != tt.want {
				t.Fatalf("unexpected bytes: got %d want %d", got, tt.want)
			}
		})
	}
}

func TestCacheStatusNilTorrentReturnsMetadataError(t *testing.T) {
	cfg := &config.Config{
		StreamCacheSize: 200 * 1024 * 1024,
		DownloadDir:     os.TempDir(),
	}
	e, _ := New(cfg)
	defer e.Close()

	e.mu.Lock()
	e.managedTorrents["dummy"] = &ManagedTorrent{}
	e.mu.Unlock()

	_, err := e.GetCacheStatus("dummy", 0, 0)
	if err == nil {
		t.Fatal("expected metadata error")
	}
	if errors.Is(err, ErrTorrentNotFound) {
		t.Fatalf("expected metadata error, got %v", err)
	}
}
