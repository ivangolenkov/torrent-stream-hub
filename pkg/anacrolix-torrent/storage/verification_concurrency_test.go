package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/metainfo"
)

// Two contributing files make the stat/publication interleaving deterministic:
// the stat hook for b runs after a's FileInfo has already been captured.
func TestVerificationPublicationConcurrentNeighborWrite(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	backend := NewFileOpts(NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, UsePartFiles: g.Some(false), NoCompletionRename: true, PieceCompletion: NewMapPieceCompletion()})
	defer backend.Close()
	info := &metainfo.Info{Name: "bundle", PieceLength: 8, Pieces: make([]byte, 40), Files: []metainfo.FileInfo{{Length: 12, Path: []string{"a"}}, {Length: 4, Path: []string{"b"}}}}
	ts, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	p0 := ts.Piece(info.Piece(0)).(*filePieceImpl)
	p1 := ts.Piece(info.Piece(1)).(*filePieceImpl)
	if _, err := p0.WriteAt([]byte("abcdefgh"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := p1.WriteAt([]byte("ijklmnop"), 0); err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1234567890, 0)
	if err := os.Chtimes(filepath.Join(dir, "bundle", "a"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := p1.BeginVerification(); err != nil {
		t.Fatal(err)
	}
	// A neighbor is allowed to finish while the hash attempt is in flight.
	// Its mtime update must not invalidate the disjoint piece's snapshot.
	if _, err := p0.WriteAt([]byte("ABCDEFGH"), 0); err != nil {
		t.Fatal(err)
	}
	stats := 0
	written := make(chan error, 1)
	p1.t.statHook = func() {
		stats++
		if stats == 2 {
			// Publication must hold a read lease across both stats and commit.
			if p1.t.files[0].payloadMu.TryLock() {
				p1.t.files[0].payloadMu.Unlock()
				t.Error("stat/publication is not fenced from neighboring writes")
			}
			go func() { _, err := p0.WriteAt([]byte("ABCDEFGH"), 0); written <- err }()
		}
	}
	if err := p1.MarkComplete(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("neighbor writer deadlocked")
	}
	p1.t.statHook = nil
	if !p1.Completion().Complete {
		t.Fatal("independent write revoked completion")
	}
}

func TestVerificationSnapshotRevokedBySamePieceWrite(t *testing.T) {
	_, _, info, ts := verifiedFixture(t)
	p := ts.Piece(info.Piece(0)).(*filePieceImpl)
	if _, err := p.WriteAt([]byte("good"), 0); err != nil {
		t.Fatal(err)
	}
	if err := p.BeginVerification(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.WriteAt([]byte("evil"), 0); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkComplete(); err == nil {
		t.Fatal("write resurrected stale hash")
	}
	if p.Completion().Complete {
		t.Fatal("changed piece trusted")
	}
	if err := p.BeginVerification(); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkNotComplete(); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkComplete(); err == nil {
		t.Fatal("invalidation resurrected stale hash")
	}
}

type invalidateOnSetCompletion struct {
	PieceCompletion
	invalidate func()
}

func (c invalidateOnSetCompletion) Persistent() bool { return false }
func (c invalidateOnSetCompletion) Set(k metainfo.PieceKey, complete bool) error {
	if complete {
		c.invalidate()
	}
	return c.PieceCompletion.Set(k, complete)
}

func TestVerificationRevocationDuringCompletionCommit(t *testing.T) {
	_, backend, info, ts := verifiedFixture(t)
	p := ts.Piece(info.Piece(0)).(*filePieceImpl)
	if _, err := p.WriteAt([]byte("good"), 0); err != nil {
		t.Fatal(err)
	}
	if err := p.BeginVerification(); err != nil {
		t.Fatal(err)
	}
	backend.opts.PieceCompletion = invalidateOnSetCompletion{backend.opts.PieceCompletion, func() {
		p.t.freshMu.Lock()
		delete(p.t.hashSnapshot, 0)
		p.t.freshMu.Unlock()
	}}
	if err := p.MarkComplete(); err == nil {
		t.Fatal("revoked snapshot published")
	}
	if ts.CachedComplete(0) {
		t.Fatal("revocation lost during hint persistence")
	}
}
