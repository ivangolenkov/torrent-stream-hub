package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRootDirectoryMoveNoReplace(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.MkdirAll("source/child", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source/child/file"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	source, err := OpenRootDirectory(root, "source")
	if err != nil {
		t.Fatal(err)
	}
	id, err := FileIdentity(source)
	source.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := MoveRootDirectoryNoReplace(root, "source", "target"); err != nil {
		t.Fatal(err)
	}
	target, err := OpenRootDirectory(root, "target")
	if err != nil {
		t.Fatal(err)
	}
	actual, err := FileIdentity(target)
	target.Close()
	if err != nil || id != actual {
		t.Fatal("rename changed identity", err)
	}
	if err := root.MkdirAll("source", 0700); err != nil {
		t.Fatal(err)
	}
	if err := MoveRootDirectoryNoReplace(root, "source", "target"); err == nil {
		t.Fatal("overwrote target")
	}
	data, err := os.ReadFile(filepath.Join(dir, "target/child/file"))
	if err != nil || string(data) != "data" {
		t.Fatal("lost data", err)
	}
	if _, err := OpenRootDirectory(root, "../outside"); !errors.Is(err, ErrUnsafeFile) {
		t.Fatal("accepted escape", err)
	}
	if err := root.Symlink("target", "link"); err == nil {
		if err := MoveRootDirectoryNoReplace(root, "link", "other"); !errors.Is(err, ErrUnsafeFile) {
			t.Fatal("moved symlink", err)
		}
	}
}
