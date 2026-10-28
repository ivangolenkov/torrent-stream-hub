package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDisablePieceSyncSkipsOnlyAutomaticFlush(t *testing.T) {
	for _, mode := range []string{"managed", "verified", "legacy", "nonpersistent"} {
		t.Run(mode, func(t *testing.T) {
			ts, p, pc, f := policyFixture(t, mode)
			p.t.client.opts.DisablePieceSync = true
			calls := 0
			injected := errors.New("explicit sync failed")
			if f != nil {
				f.syncIO = func() error { calls++; return injected }
			} else {
				p.t.io = checkpointFlush{p.t.io, func(string) error { calls++; return injected }}
			}
			if err := p.MarkComplete(); err != nil {
				t.Fatal(err)
			}
			hint, err := pc.Get(p.pieceKey())
			if err != nil || !hint.Complete || !p.Completion().Complete || calls != 0 {
				t.Fatal("disabled sync still flushed or failed to publish", hint, err, calls)
			}
			if io := ts.IOTimings(); io.CompletionSyncCount != 0 || io.CompletionSyncErrors != 0 || io.CompletionSyncActive != 0 || io.CompletionSyncNS != 0 {
				t.Fatal("skipped Flush changed automatic sync counters", io)
			}
			if err := p.Flush(); !errors.Is(err, injected) || calls != 1 {
				t.Fatal("explicit Flush was suppressed", err, calls)
			}
		})
	}
}

func TestDisablePieceSyncPreservesSetErrors(t *testing.T) {
	ts, p, pc, _ := policyFixture(t, "managed")
	p.t.client.opts.DisablePieceSync = true
	pc.setError = errors.New("Set failed")
	if err := p.MarkComplete(); !errors.Is(err, pc.setError) || ts.CachedComplete(0) {
		t.Fatal("skip sync suppressed Set failure", err)
	}
}

func TestDisablePieceSyncPreservesCheckpointSync(t *testing.T) {
	for _, stage := range []string{"payload", "directory"} {
		t.Run(stage, func(t *testing.T) {
			ts, p, _, _ := policyFixture(t, "managed")
			p.t.client.opts.DisablePieceSync = true
			if err := p.MarkComplete(); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("mandatory checkpoint sync failed")
			if stage == "payload" {
				p.t.io = checkpointFlush{p.t.io, func(string) error { return injected }}
			} else {
				p.t.checkpointSyncDir = func(*os.Root, string) error { return injected }
			}
			published := false
			err := ts.WithCheckpoint(context.Background(), []byte{1}, func(FileCheckpoint) error { published = true; return nil })
			if !errors.Is(err, injected) || published {
				t.Fatal("disabled checkpoint sync", err, published)
			}
		})
	}
}

func TestDisablePieceSyncPreservesPromotionSync(t *testing.T) {
	dir, backend, info, ts := verifiedFixture(t)
	backend.opts.DisablePieceSync = true
	backend.opts.PieceCompletion = persistentTestCompletion{NewMapPieceCompletion()}
	if err := os.WriteFile(filepath.Join(dir, "data.part"), []byte("good"), 0600); err != nil {
		t.Fatal(err)
	}
	p := ts.Piece(info.Piece(0)).(*filePieceImpl)
	if err := p.BeginVerification(); err != nil {
		t.Fatal(err)
	}
	p.t.io = failPublicationFlush{p.t.io}
	if err := p.MarkComplete(); !errors.Is(err, os.ErrPermission) {
		t.Fatal("promotion sync error hidden", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "data")); !os.IsNotExist(err) {
		t.Fatal("promoted without sync", err)
	}
	if ts.IOTimings().CompletionSyncCount != 0 {
		t.Fatal("automatic piece sync was not skipped")
	}
}
