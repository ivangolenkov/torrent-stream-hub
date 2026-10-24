package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/metainfo"
)

func plainNameBackend(t *testing.T, dir string) ClientImplCloser {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	backend := NewFileOpts(NewFileClientOpts{ClientBaseDir: dir, RootFS: root,
		UsePartFiles: g.Some(false), ImportPartFiles: true, VerifyOnOpen: true,
		PieceCompletion: persistentTestCompletion{NewMapPieceCompletion()}})
	t.Cleanup(func() { backend.Close() })
	return backend
}

func TestPlainNamesDuringWriteVerificationAndReopen(t *testing.T) {
	dir := t.TempDir()
	backend := plainNameBackend(t, dir)
	info := &metainfo.Info{Name: "movie.mkv", Length: 8, PieceLength: 4, Pieces: make([]byte, 40)}
	ts, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	for i, content := range []string{"abcd", "efgh"} {
		p := ts.Piece(info.Piece(i)).(*filePieceImpl)
		if _, err := p.WriteAt([]byte(content), 0); err != nil {
			t.Fatal(err)
		}
		if p.Completion().Complete {
			t.Fatal("ordinary filename granted readiness before verification")
		}
		if _, err := os.Stat(filepath.Join(dir, info.Name)); err != nil {
			t.Fatal("download does not use metadata filename", err)
		}
		if err := p.BeginVerification(); err != nil {
			t.Fatal(err)
		}
		if err := p.MarkComplete(); err != nil {
			t.Fatal(err)
		}
	}
	if err := ts.Piece(info.Piece(0)).MarkNotComplete(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, info.Name+".part")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completion or invalidation created .part", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, info.Name)); err != nil || string(data) != "abcdefgh" {
		t.Fatalf("payload changed: %q %v", data, err)
	}
	if err := ts.Close(); err != nil {
		t.Fatal(err)
	}
	ts, err = backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	if ts.FreshComplete(1) {
		t.Fatal("reopen trusted previous completion")
	}
}

func TestImportPartialFilesResumesWithoutCopyOrTrust(t *testing.T) {
	dir := t.TempDir()
	backend := plainNameBackend(t, dir)
	// The first file has already been renamed by an interrupted import.
	info := &metainfo.Info{Name: "bundle", PieceLength: 4, Pieces: make([]byte, 40), Files: []metainfo.FileInfo{
		{Length: 4, Path: []string{"first.mkv"}}, {Length: 4, Path: []string{"second.mkv"}},
	}}
	if err := os.Mkdir(filepath.Join(dir, "bundle"), 0700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(dir, "bundle", "first.mkv")
	second := filepath.Join(dir, "bundle", "second.mkv")
	if err := os.WriteFile(first, []byte("done"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second+".part", []byte("ab"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(second + ".part")
	if err != nil {
		t.Fatal(err)
	}
	// File.Stat captures identity before rename on Windows too; path-based
	// os.Stat can defer identity lookup until os.SameFile is called.
	before, err := f.Stat()
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	ts, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	after, err := os.Stat(second)
	if err != nil || !os.SameFile(before, after) || after.Size() != 2 {
		t.Fatalf("partial payload was not preserved by rename: %v", err)
	}
	if _, err := os.Stat(second + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old name remains", err)
	}
	if ts.FreshComplete(0) || ts.FreshComplete(1) {
		t.Fatal("renaming granted verification")
	}
	p := ts.Piece(info.Piece(1))
	if _, err := p.WriteAt([]byte("cd"), 2); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(second); err != nil || string(data) != "abcd" {
		t.Fatalf("resume discarded downloaded bytes: %q %v", data, err)
	}
}

func TestImportPartFilesConflictPreflightPreservesAllFiles(t *testing.T) {
	dir := t.TempDir()
	backend := plainNameBackend(t, dir)
	info := &metainfo.Info{Name: "bundle", PieceLength: 4, Pieces: make([]byte, 40), Files: []metainfo.FileInfo{
		{Length: 4, Path: []string{"first"}}, {Length: 4, Path: []string{"second"}},
	}}
	if err := os.Mkdir(filepath.Join(dir, "bundle"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first.part", "second.part", "second"} {
		if err := os.WriteFile(filepath.Join(dir, "bundle", name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{}); err == nil || !strings.Contains(err.Error(), "both files preserved") {
		t.Fatal("ambiguous legacy payload was accepted", err)
	}
	for _, name := range []string{"first.part", "second.part", "second"} {
		if data, err := os.ReadFile(filepath.Join(dir, "bundle", name)); err != nil || string(data) != name {
			t.Fatalf("conflict changed %s: %q %v", name, data, err)
		}
	}
}

func TestPlainNamesRespectMetadataPartExtension(t *testing.T) {
	for _, second := range []string{"a.part", "a.part/child"} {
		t.Run(second, func(t *testing.T) {
			dir := t.TempDir()
			backend := plainNameBackend(t, dir)
			info := &metainfo.Info{Name: "bundle", PieceLength: 4, Pieces: make([]byte, 20), Files: []metainfo.FileInfo{
				{Length: 1, Path: []string{"a"}}, {Length: 1, Path: strings.Split(second, "/")},
			}}
			path := filepath.Join(dir, "bundle", filepath.FromSlash(second))
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("b"), 0600); err != nil {
				t.Fatal(err)
			}
			ts, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
			if err != nil {
				t.Fatal(err)
			}
			defer ts.Close()
			if _, err := os.Stat(filepath.Join(dir, "bundle", "a")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("real metadata file was treated as a sidecar", err)
			}
		})
	}
}

func TestImportPartFilesRejectsUnsafeLinksAndCancellation(t *testing.T) {
	for _, mode := range []string{"symlink", "hardlink", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			backend := plainNameBackend(t, dir)
			outside := filepath.Join(t.TempDir(), "source")
			if err := os.WriteFile(outside, []byte("safe"), 0600); err != nil {
				t.Fatal(err)
			}
			part := filepath.Join(dir, "movie.part")
			var err error
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "symlink":
				err = os.Symlink(outside, part)
			case "hardlink":
				err = os.Link(outside, part)
			case "cancelled":
				err = os.WriteFile(part, []byte("safe"), 0600)
				cancel()
			}
			if err != nil {
				t.Skip(err)
			}
			info := &metainfo.Info{Name: "movie", Length: 4, PieceLength: 4, Pieces: make([]byte, 20)}
			if _, err := backend.OpenTorrent(ctx, info, metainfo.Hash{}); err == nil {
				t.Fatal("unsafe or cancelled import accepted")
			}
			if _, err := os.Lstat(part); err != nil {
				t.Fatal("source removed", err)
			}
			if data, err := os.ReadFile(outside); err != nil || string(data) != "safe" {
				t.Fatal("outside data changed", err)
			}
		})
	}
}
