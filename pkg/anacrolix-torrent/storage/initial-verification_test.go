package storage

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/metainfo"
)

func TestInitialIncompleteEvidence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sizes []int
		part  bool
		want  []PieceRange
	}{
		{"missing", []int{-1, -1}, false, []PieceRange{{0, 4}}},
		{"present-not-trusted", []int{6, 10}, false, nil},
		{"boundary-missing", []int{6, -1}, false, []PieceRange{{1, 4}}},
		{"truncated", []int{4, 10}, false, []PieceRange{{1, 2}}},
		{"partial-tail", []int{6, 7}, false, []PieceRange{{3, 4}}},
		{"part-files", []int{6, 10}, true, nil},
		{"part-truncated", []int{6, 3}, true, []PieceRange{{2, 4}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "bundle"), 0700); err != nil {
				t.Fatal(err)
			}
			for i, size := range tc.sizes {
				if size < 0 {
					continue
				}
				name := string(rune('a' + i))
				if tc.part {
					name += ".part"
				}
				if err := os.WriteFile(filepath.Join(dir, "bundle", name), make([]byte, size), 0600); err != nil {
					t.Fatal(err)
				}
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			backend := NewFileOpts(NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, UsePartFiles: g.Some(tc.part), NoCompletionRename: true, PieceCompletion: NewMapPieceCompletion()})
			defer backend.Close()
			info := &metainfo.Info{Name: "bundle", PieceLength: 4, Pieces: make([]byte, 80), Files: []metainfo.FileInfo{{Length: 6, Path: []string{"a"}}, {Length: 10, Path: []string{"b"}}}}
			ts, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
			if err != nil {
				t.Fatal(err)
			}
			defer ts.Close()
			if !reflect.DeepEqual(ts.InitialIncomplete, tc.want) {
				t.Fatalf("got %v, want %v", ts.InitialIncomplete, tc.want)
			}
			for i := 0; i < 4; i++ {
				if ts.CachedComplete(i) {
					t.Fatal("metadata evidence granted trust")
				}
			}
		})
	}
}

func TestInitialIncompleteDoesNotHideEvidenceError(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	backend := NewFileOpts(NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, UsePartFiles: g.Some(false), NoCompletionRename: true, PieceCompletion: NewMapPieceCompletion(), IOGuard: func(string) error { return os.ErrPermission }})
	defer backend.Close()
	info := &metainfo.Info{Name: "data", Length: 8, PieceLength: 4, Pieces: make([]byte, 40)}
	ts, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	if len(ts.InitialIncomplete) != 0 {
		t.Fatal("access error treated as missing")
	}
}

func TestInitialIncompleteLargeGeometryHasPerFileCost(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	events := 0
	backend := NewFileOpts(NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, UsePartFiles: g.Some(false), NoCompletionRename: true, PieceCompletion: NewMapPieceCompletion(), IOEvent: func(string, string) { events++ }})
	defer backend.Close()
	info := &metainfo.Info{Name: "bundle", PieceLength: 2 << 20, Pieces: make([]byte, 7233*20)}
	for i := 0; i < 10; i++ {
		length := int64(723 * 2 << 20)
		if i == 9 {
			length += 3 * 2 << 20
		}
		info.Files = append(info.Files, metainfo.FileInfo{Length: length, Path: []string{string(rune('a' + i))}})
	}
	ts, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	if !reflect.DeepEqual(ts.InitialIncomplete, []PieceRange{{0, 7233}}) {
		t.Fatal(ts.InitialIncomplete)
	}
	if events > 120 {
		t.Fatalf("per-piece metadata amplification: %d events for 10 files", events)
	}
	if _, err := os.Stat(filepath.Join(dir, "bundle")); !os.IsNotExist(err) {
		t.Fatal("planning created payload", err)
	}
}
