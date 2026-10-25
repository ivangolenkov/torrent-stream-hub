package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

func TestNativeSeparateMountRenameAndVerificationResources(t *testing.T) {
	left := t.TempDir()
	right, err := os.MkdirTemp("/dev/shm", "tsh-storage-")
	if err != nil {
		t.Skip("second writable mount unavailable:", err)
	}
	t.Cleanup(func() { os.RemoveAll(right) })
	a, err := os.Open(left)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := os.Open(right)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	aID, err := FilesystemIdentity(a)
	if err != nil {
		t.Fatal(err)
	}
	bID, err := FilesystemIdentity(b)
	if err != nil {
		t.Fatal(err)
	}
	if aID == bID {
		t.Skip("/dev/shm and temporary directory share a filesystem")
	}
	root, err := os.OpenRoot("/")
	if err != nil {
		t.Fatal(err)
	}
	backend := NewFileOpts(NewFileClientOpts{ClientBaseDir: "/", RootFS: root, VerifyOnOpen: true, NoCompletionRename: true, PieceCompletion: NewMapPieceCompletion(), TorrentPathResolver: func(base string, _ *metainfo.Info, _ metainfo.Hash) (string, []string, error) {
		return base, []string{strings.TrimPrefix(filepath.Join(left, "one"), "/"), strings.TrimPrefix(filepath.Join(right, "two"), "/")}, nil
	}})
	defer backend.Close()
	for _, path := range []string{filepath.Join(left, "one"), filepath.Join(right, "two")} {
		if err := os.WriteFile(path, []byte("ab"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	info := &metainfo.Info{Name: "boundary", PieceLength: 4, Pieces: make([]byte, 20), Files: []metainfo.FileInfo{{Length: 2, Path: []string{"one"}}, {Length: 2, Path: []string{"two"}}}}
	impl, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer impl.Close()
	keys, err := impl.Piece(info.Piece(0)).(interface{ VerificationResources() ([]string, error) }).VerificationResources()
	if err != nil || len(keys) != 2 {
		t.Fatal("boundary did not reserve both mounts", keys, err)
	}
	err = MoveRootDirectoryNoReplace(root, strings.TrimPrefix(left, "/"), strings.TrimPrefix(filepath.Join(right, "moved"), "/"))
	if !IsCrossDeviceRename(err) {
		t.Fatal("native cross-filesystem rename not classified", err)
	}
	if content, err := os.ReadFile(filepath.Join(left, "one")); err != nil || string(content) != "ab" {
		t.Fatal("EXDEV changed source", err)
	}
	if _, err := os.Stat(filepath.Join(right, "moved")); !os.IsNotExist(err) {
		t.Fatal("EXDEV published a target", err)
	}
}
