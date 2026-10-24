package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

func TestVerificationBookkeepingAndRefreshAreLinear(t *testing.T) {
	const pieces = 1000
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data"), make([]byte, pieces), 0600); err != nil {
		t.Fatal(err)
	}
	backend := NewFileOpts(NewFileClientOpts{ClientBaseDir: dir, VerifyOnOpen: true, NoCompletionRename: true})
	defer backend.Close()
	info := &metainfo.Info{Name: "data", Length: pieces, PieceLength: 1, Pieces: make([]byte, pieces*20)}
	ts, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	fts := ts.Piece(info.Piece(0)).(*filePieceImpl).t
	stats := 0
	fts.statHook = func() { stats++ }
	for index := 0; index < pieces; index++ {
		p := ts.Piece(info.Piece(index)).(*filePieceImpl)
		if err := p.BeginVerification(); err != nil {
			t.Fatal(err)
		}
		if err := p.MarkComplete(); err != nil {
			t.Fatal(err)
		}
	}
	if stats > 4*pieces+2 {
		t.Fatalf("quadratic completion bookkeeping: %d stats", stats)
	}
	stats = 0
	ts.RefreshFresh(0, pieces)
	for index := 0; index < pieces; index++ {
		if !ts.CachedComplete(index) {
			t.Fatalf("piece %d revoked", index)
		}
	}
	if stats > 2 {
		t.Fatalf("refresh statted one file %d times", stats)
	}
}
