package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

func verifiedFixture(t *testing.T) (string, *fileClientImpl, *metainfo.Info, TorrentImpl) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	backend := NewFileOpts(NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true}).(*fileClientImpl)
	t.Cleanup(func() { backend.Close() })
	info := &metainfo.Info{Name: "data", Length: 4, PieceLength: 4, Pieces: make([]byte, 20)}
	ts, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ts.Close() })
	return dir, backend, info, ts
}

func TestVerifiedCompletionIgnoresPersistentHintAndReopen(t *testing.T) {
	dir, backend, info, ts := verifiedFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("evil"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := backend.opts.PieceCompletion.Set(metainfo.PieceKey{}, true); err != nil {
		t.Fatal(err)
	}
	p := ts.Piece(info.Piece(0)).(*filePieceImpl)
	if p.Completion().Complete || ts.FreshComplete(0) {
		t.Fatal("final name/size or old completion trusted")
	}
	if err := p.MarkComplete(); err == nil {
		t.Fatal("accepted completion without verification snapshot")
	}
	// The hash caller, not the storage backend, compares content with metadata.
	if err := p.BeginVerification(); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkComplete(); err != nil {
		t.Fatal(err)
	}
	if !p.Completion().Complete {
		t.Fatal("fresh hash result rejected")
	}
	reopened, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.FreshComplete(0) {
		t.Fatal("reopen reused previous storage generation")
	}
}

func TestVerifiedCompletionRevokedOnReplacementAndHashMutation(t *testing.T) {
	dir, _, info, ts := verifiedFixture(t)
	path := filepath.Join(dir, "data")
	if err := os.WriteFile(path, []byte("good"), 0600); err != nil {
		t.Fatal(err)
	}
	p := ts.Piece(info.Piece(0)).(*filePieceImpl)
	if err := p.BeginVerification(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("evil"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkComplete(); err == nil {
		t.Fatal("replacement during hashing accepted")
	}
	if err := p.BeginVerification(); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkComplete(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".old", path); err != nil {
		t.Fatal(err)
	}
	if p.Completion().Complete || ts.FreshComplete(0) {
		t.Fatal("replacement retained verified trust")
	}
	if err := ts.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.WriteAt([]byte("oops"), 0); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("write after close: %v", err)
	}
}

func TestRootedWriterRejectsSymlinkAndHardlink(t *testing.T) {
	for _, kind := range []string{"parent-symlink", "file-symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			dir, _, info, ts := verifiedFixture(t)
			outside := t.TempDir()
			victim := filepath.Join(outside, "data.part")
			if err := os.WriteFile(victim, []byte("safe"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			if kind == "parent-symlink" {
				// Change the already resolved storage path to an escaping parent.
				p := ts.Piece(info.Piece(0)).(*filePieceImpl)
				p.t.files[0].safeOsPath = filepath.Join(dir, "alias", "data")
				err = os.Symlink(outside, filepath.Join(dir, "alias"))
			} else if kind == "file-symlink" {
				err = os.Symlink(victim, filepath.Join(dir, "data.part"))
			} else {
				err = os.Link(victim, filepath.Join(dir, "data.part"))
			}
			if err != nil {
				t.Skip(err)
			}
			if _, err := ts.Piece(info.Piece(0)).WriteAt([]byte("evil"), 0); err == nil {
				t.Fatal("unsafe writer admitted")
			}
			data, err := os.ReadFile(victim)
			if err != nil || string(data) != "safe" {
				t.Fatalf("outside data changed: %q %v", data, err)
			}
		})
	}
}

func TestRootedIOCancellationAndNoReplace(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ctx, cancel := context.WithCancel(context.Background())
	r := rootFileIo{root: root, ctx: ctx}
	if err := root.WriteFile("from", []byte("good"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("to", []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.rename(filepath.Join(dir, "from"), filepath.Join(dir, "to")); err == nil {
		t.Fatal("occupied target replaced")
	}
	f, err := r.openForRead(filepath.Join(dir, "from"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cancel()
	if _, err := f.Read(make([]byte, 4)); !errors.Is(err, context.Canceled) {
		t.Fatalf("read ignored cancellation: %v", err)
	}
	data, err := root.ReadFile("to")
	if err != nil || string(data) != "safe" {
		t.Fatal("target changed")
	}
}

func TestVerifiedMultiFileBoundaryAndZeroLength(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	backend := NewFileOpts(NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true})
	defer backend.Close()
	info := &metainfo.Info{Name: "bundle", PieceLength: 4, Pieces: make([]byte, 40), Files: []metainfo.FileInfo{
		{Length: 3, Path: []string{"a"}}, {Length: 3, Path: []string{"b"}}, {Length: 0, Path: []string{"empty"}},
	}}
	ts, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	p0 := ts.Piece(info.Piece(0)).(*filePieceImpl)
	p1 := ts.Piece(info.Piece(1)).(*filePieceImpl)
	if _, err := p0.WriteAt([]byte("abcd"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := p1.WriteAt([]byte("ef"), 0); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*filePieceImpl{p1, p0} {
		if err := p.BeginVerification(); err != nil {
			t.Fatal(err)
		}
		if err := p.MarkComplete(); err != nil {
			t.Fatal(err)
		}
	}
	if !p0.Completion().Complete || !p1.Completion().Complete {
		t.Fatal("boundary completion lost during promotion")
	}
	if st, err := os.Stat(filepath.Join(dir, "bundle", "empty")); err != nil || st.Size() != 0 {
		t.Fatal("zero file missing", err)
	}
	path := filepath.Join(dir, "bundle", "a")
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if p0.Completion().Complete {
		t.Fatal("shared boundary still trusted after mutation")
	}
	if !p1.Completion().Complete {
		t.Fatal("unaffected piece invalidated")
	}
}

func TestRootedStorageRejectsOverlappingMetadata(t *testing.T) {
	for _, paths := range [][]string{{"a", "a.part"}, {"a", "a/b"}, {"A", "a"}} {
		t.Run(paths[1], func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			backend := NewFileOpts(NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true})
			defer backend.Close()
			info := &metainfo.Info{Name: "bundle", PieceLength: 4, Pieces: make([]byte, 20)}
			for _, path := range paths {
				info.Files = append(info.Files, metainfo.FileInfo{Length: 1, Path: strings.Split(path, "/")})
			}
			if _, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{}); err == nil {
				t.Fatal("ambiguous paths accepted")
			}
		})
	}
}
