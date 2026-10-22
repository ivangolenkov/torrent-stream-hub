//go:build !noboltdb && !wasm

package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

func TestVerifiedOpenIgnoresReopenedBoltCompletion(t *testing.T) {
	dir := t.TempDir()
	completion, err := NewBoltPieceCompletion(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := metainfo.PieceKey{}
	if err := completion.Set(key, true); err != nil {
		t.Fatal(err)
	}
	if err := completion.Close(); err != nil {
		t.Fatal(err)
	}
	completion, err = NewBoltPieceCompletion(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, err := completion.Get(key)
	if err != nil || !c.Complete {
		completion.Close()
		t.Fatal("persistent fixture lost", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		completion.Close()
		t.Fatal(err)
	}
	backend := NewFileOpts(NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, PieceCompletion: completion})
	defer backend.Close()
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("evil"), 0600); err != nil {
		t.Fatal(err)
	}
	info := &metainfo.Info{Name: "data", Length: 4, PieceLength: 4, Pieces: make([]byte, 20)}
	ts, err := backend.OpenTorrent(context.Background(), info, key.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	if ts.Piece(info.Piece(0)).Completion().Complete || ts.FreshComplete(0) {
		t.Fatal("old Bolt mark bypassed verification")
	}
}
