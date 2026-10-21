package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

type blockingFileCompletion struct {
	PieceCompletion
	block   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (pc *blockingFileCompletion) Set(key metainfo.PieceKey, complete bool) error {
	if pc.block.Load() {
		close(pc.entered)
		<-pc.release
	}
	return pc.PieceCompletion.Set(key, complete)
}

func TestFileCloseWaitsForMutationAndRejectsLateHashResult(t *testing.T) {
	dir := t.TempDir()
	pc := &blockingFileCompletion{PieceCompletion: NewMapPieceCompletion(), entered: make(chan struct{}), release: make(chan struct{})}
	client := NewFileOpts(NewFileClientOpts{ClientBaseDir: dir, PieceCompletion: pc})
	defer client.Close()
	info := &metainfo.Info{Name: "payload", Length: 4, PieceLength: 4, Pieces: make([]byte, 20)}
	store, err := client.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	piece := store.Piece(info.Piece(0))
	path := filepath.Join(dir, "payload")
	if err := os.WriteFile(path, []byte("old!"), 0600); err != nil {
		t.Fatal(err)
	}
	pc.block.Store(true)
	markDone := make(chan error, 1)
	go func() { markDone <- piece.MarkNotComplete() }()
	<-pc.entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- store.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close passed in-flight mutation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(pc.release)
	if err := <-markDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	// Application cleanup/re-add can now safely install a new payload at this path.
	if err := os.WriteFile(path, []byte("new!"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, op := range []func() error{piece.MarkNotComplete, piece.MarkComplete} {
		if err := op(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("late mutation: %v", err)
		}
	}
	if _, err := piece.WriteAt([]byte("bad!"), 0); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("late write: %v", err)
	}
	if c := piece.Completion(); !errors.Is(c.Err, os.ErrClosed) {
		t.Fatalf("late completion: %+v", c)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "new!" {
		t.Fatalf("replacement changed: %q %v", got, err)
	}
}
