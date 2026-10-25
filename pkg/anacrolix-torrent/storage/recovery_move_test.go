package storage

import (
	"context"
	"errors"
	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/metainfo"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryMoveNoReplaceAcrossDirectories(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.MkdirAll("protected", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source"), []byte("only copy"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := OpenRootFile(root, "source", os.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	before, err := FileIdentity(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := MoveRootNoReplace(root, "source", "protected/alternative"); err != nil {
		t.Fatal(err)
	}
	f, err = OpenRootFile(root, "protected/alternative", os.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	after, err := FileIdentity(f)
	f.Close()
	if err != nil || before != after {
		t.Fatal("identity changed", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source"), []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := MoveRootNoReplace(root, "source", "protected/alternative"); err == nil {
		t.Fatal("overwrote alternative")
	}
	for path, want := range map[string]string{"source": "foreign", "protected/alternative": "only copy"} {
		data, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil || string(data) != want {
			t.Fatalf("lost data %s: %q %v", path, data, err)
		}
	}
	if err := SyncRootPublication(root, "protected/alternative"); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryReadOnlyDoesNotCreateZeroFileOrSidecar(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	backend := NewFileOpts(NewFileClientOpts{ClientBaseDir: dir, RootFS: root, ReadOnly: true, UsePartFiles: g.Some(false), VerifyOnOpen: true, NoCompletionRename: true, PieceCompletion: NewMapPieceCompletion()})
	defer backend.Close()
	ts, err := backend.OpenTorrent(context.Background(), &metainfo.Info{Name: "empty", PieceLength: 4}, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("read-only analysis mutated storage: %v %v", entries, err)
	}
	_, err = (rootFileIo{root: root, readOnly: true}).openForWrite(filepath.Join(dir, "empty"), 0)
	if !errors.Is(err, os.ErrPermission) {
		t.Fatal("read-only write admitted", err)
	}
}
