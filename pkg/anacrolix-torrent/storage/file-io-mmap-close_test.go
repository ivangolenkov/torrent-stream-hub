//go:build !wasm

package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/go-quicktest/qt"
)

// Check both resources explicitly: Unix permits unlinking an open mapped file,
// so successful TempDir cleanup alone would not catch the Windows regression.
func checkMmapReleased(t *testing.T, mapping *fileMmap) {
	t.Helper()
	_, err := mapping.f.Stat()
	qt.Check(t, qt.IsTrue(errors.Is(err, os.ErrClosed)))
	qt.Check(t, qt.IsNil(mapping.m))
}

func TestMmapFileIoCloseReleasesCachedFiles(t *testing.T) {
	dir := t.TempDir()
	backend := &mmapFileIo{}
	t.Cleanup(func() { qt.Check(t, qt.IsNil(backend.Close())) })
	paths := []string{filepath.Join(dir, "read"), filepath.Join(dir, "write")}
	var mappings []*fileMmap
	for i, path := range paths {
		qt.Assert(t, qt.IsNil(os.WriteFile(path, []byte("data"), 0600)))
		var handle fileWriter
		var err error
		if i == 0 {
			handle, err = backend.openReadOnly(path)
		} else {
			handle, err = backend.openForWrite(path, 4)
		}
		qt.Assert(t, qt.IsNil(err))
		t.Cleanup(func() { qt.Check(t, qt.IsNil(handle.Close())) })
		qt.Assert(t, qt.IsNil(handle.Close()))
		mapping := backend.paths[path]
		_, err = mapping.f.Stat()
		qt.Assert(t, qt.IsNil(err)) // Still owned by the cache.
		mappings = append(mappings, mapping)
	}
	qt.Assert(t, qt.IsNil(backend.Close()))
	qt.Check(t, qt.IsNil(backend.Close()))
	for i, mapping := range mappings {
		checkMmapReleased(t, mapping)
		qt.Check(t, qt.IsNil(os.Remove(paths[i])))
	}
	_, err := backend.openForSharedRead(paths[0])
	qt.Check(t, qt.IsTrue(errors.Is(err, os.ErrClosed)))
	_, err = backend.openForRead(paths[0])
	qt.Check(t, qt.IsTrue(errors.Is(err, os.ErrClosed)))
	_, err = backend.openForWrite(paths[0], 4)
	qt.Check(t, qt.IsTrue(errors.Is(err, os.ErrClosed)))
	qt.Check(t, qt.IsTrue(errors.Is(backend.flush(paths[0], 0, 4), os.ErrClosed)))
	qt.Check(t, qt.IsTrue(errors.Is(backend.rename(paths[0], paths[1]), os.ErrClosed)))
}

func TestMmapFileIoCloseWithLiveHandles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload")
	qt.Assert(t, qt.IsNil(os.WriteFile(path, []byte("data"), 0600)))
	backend := &mmapFileIo{}
	t.Cleanup(func() { qt.Check(t, qt.IsNil(backend.Close())) })
	writer, err := backend.openForWrite(path, 4)
	qt.Assert(t, qt.IsNil(err))
	t.Cleanup(func() { qt.Check(t, qt.IsNil(writer.Close())) })
	reader, err := backend.openReadOnly(path)
	qt.Assert(t, qt.IsNil(err))
	t.Cleanup(func() { qt.Check(t, qt.IsNil(reader.Close())) })
	mapping := reader.f
	closed := make(chan error, 1)
	go func() { closed <- backend.Close() }()
	select {
	case err := <-closed:
		qt.Assert(t, qt.IsNil(err))
	case <-time.After(5 * time.Second):
		t.Fatal("cache Close blocked on a live handle")
	}
	// Removing the cache reference must neither invalidate existing handles nor
	// force them to close before the cache itself can finish closing.
	n, err := writer.WriteAt([]byte("next"), 0)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(n, 4))
	qt.Assert(t, qt.IsNil(writer.Close()))
	buf := make([]byte, 3)
	n, err = reader.ReadAt(buf, 0)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(n, 3))
	qt.Check(t, qt.Equals(string(buf), "nex"))
	qt.Assert(t, qt.IsNil(reader.Close()))
	checkMmapReleased(t, mapping)
	qt.Check(t, qt.IsNil(os.Remove(path)))
}

func TestFileTorrentCloseReleasesMmapCache(t *testing.T) {
	dir := t.TempDir()
	client := NewFileOpts(NewFileClientOpts{ClientBaseDir: dir})
	t.Cleanup(func() { qt.Check(t, qt.IsNil(client.Close())) })
	info := &metainfo.Info{Name: "payload", Length: 4, PieceLength: 4, Pieces: make([]byte, 20)}
	store, err := client.OpenTorrent(context.Background(), info, metainfo.Hash{})
	qt.Assert(t, qt.IsNil(err))
	t.Cleanup(func() { qt.Check(t, qt.IsNil(store.Close())) })
	piece := store.Piece(info.Piece(0)).(*filePieceImpl)
	// Select mmap explicitly so this regression also runs when the default
	// backend is overridden to classic through the environment.
	backend := &mmapFileIo{}
	piece.t.io = backend
	path := filepath.Join(dir, "payload")
	qt.Assert(t, qt.IsNil(os.WriteFile(path, []byte("data"), 0600)))
	buf := make([]byte, 4)
	n, err := piece.ReadAt(buf, 0)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(n, 4))
	qt.Check(t, qt.Equals(string(buf), "data"))
	mapping := backend.paths[path]
	qt.Assert(t, qt.IsNotNil(mapping))
	qt.Assert(t, qt.IsNil(store.Close()))
	qt.Check(t, qt.IsNil(store.Close()))
	checkMmapReleased(t, mapping)
	qt.Check(t, qt.IsNil(os.Remove(path)))
	_, err = piece.ReadAt(buf, 0)
	qt.Check(t, qt.IsTrue(errors.Is(err, os.ErrClosed)))
}
