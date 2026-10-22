package torrent

import (
	"context"
	"crypto/sha1"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

func TestVerifiedReaderAndBitfieldRejectCorruptFinal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data")
	if err := os.WriteFile(path, []byte("evil"), 0600); err != nil {
		t.Fatal(err)
	}
	h := sha1.Sum([]byte("good"))
	info := metainfo.Info{Name: "data", Length: 4, PieceLength: 4, Pieces: h[:]}
	encoded, err := bencode.Marshal(&info)
	if err != nil {
		t.Fatal(err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: encoded}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	completion := storage.NewMapPieceCompletion()
	if err := completion.Set(metainfo.PieceKey{InfoHash: mi.HashInfoBytes(), Index: 0}, true); err != nil {
		t.Fatal(err)
	}
	backend := storage.NewFileOpts(storage.NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, NoCompletionRename: true, PieceCompletion: completion})
	defer backend.Close()
	cfg := TestingConfig(t)
	cfg.DefaultStorage = backend
	cfg.NoUpload = true
	cfg.DisableTrackers = true
	cl, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	spec := TorrentSpecFromMetaInfo(mi)
	spec.DisallowDataDownload = true
	tor, _, err := cl.AddTorrentSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tor.VerifyDataContext(ctx); err != nil {
		t.Fatal(err)
	}
	assertNoHave := func() {
		t.Helper()
		cl.lock()
		defer cl.unlock()
		if tor.haveAllPieces() || tor.bitfield()[0] || tor.pieceComplete(0) {
			t.Fatal("unverified piece advertised")
		}
	}
	assertNoHave()
	r := tor.NewReader()
	defer r.Close()
	r.SetResponsive()
	readCtx, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	if n, err := r.ReadContext(readCtx, make([]byte, 4)); n != 0 || err == nil {
		t.Fatalf("responsive read exposed corrupt bytes: n=%d err=%v", n, err)
	}
	// Replace with correct data, recheck, then replace again after verification.
	if err := os.WriteFile(path, []byte("good"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := tor.VerifyDataContext(ctx); err != nil {
		t.Fatal(err)
	}
	cl.lock()
	ready := tor.haveAllPieces() && tor.bitfield()[0]
	cl.unlock()
	if !ready {
		t.Fatal("correct data not verified")
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("evil"), 0600); err != nil {
		t.Fatal(err)
	}
	assertNoHave()
}
