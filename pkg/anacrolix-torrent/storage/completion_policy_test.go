package storage

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

type policyCompletion struct {
	PieceCompletion
	persistent bool
	setError   error
	sets       atomic.Int64
}

func (c *policyCompletion) Persistent() bool { return c.persistent }
func (c *policyCompletion) Set(k metainfo.PieceKey, v bool) error {
	c.sets.Add(1)
	if c.setError != nil {
		return c.setError
	}
	return c.PieceCompletion.Set(k, v)
}

func policyFixture(t *testing.T, mode string) (TorrentImpl, *filePieceImpl, *policyCompletion, *managedTestFile) {
	t.Helper()
	var ts TorrentImpl
	var p *filePieceImpl
	var f *managedTestFile
	if mode == "managed" {
		var info *metainfo.Info
		ts, _, info, f = managedRangeFixture(t, false)
		p = ts.Piece(info.Piece(0)).(*filePieceImpl)
	} else {
		ts, p, _ = writeConcurrencyBackendFixture(t, &metainfo.Info{Name: "data", Length: 8, PieceLength: 4, Pieces: make([]byte, 40)}, mode != "legacy", false)
	}
	pc := &policyCompletion{PieceCompletion: NewMapPieceCompletion(), persistent: mode != "nonpersistent"}
	p.t.client.opts.PieceCompletion = pc
	if p.t.client.opts.VerifyOnOpen {
		if err := p.BeginVerification(); err != nil {
			t.Fatal(err)
		}
	}
	return ts, p, pc, f
}

func TestLegacyCompletionSetPrecedesBestEffortSync(t *testing.T) {
	for _, mode := range []string{"managed", "verified", "legacy"} {
		for _, failSync := range []bool{false, true} {
			name := mode + "/success"
			if failSync {
				name = mode + "/disk-full"
			}
			t.Run(name, func(t *testing.T) {
				ts, p, pc, f := policyFixture(t, mode)
				var logs bytes.Buffer
				p.t.client.opts.Logger = slog.New(slog.NewTextHandler(&logs, nil))
				entered, release := make(chan struct{}), make(chan struct{})
				var once, unblock sync.Once
				defer unblock.Do(func() { close(release) })
				if f != nil {
					f.syncIO = func() error {
						once.Do(func() { close(entered) })
						<-release
						if failSync {
							return syscall.ENOSPC
						}
						return f.managedPayloadFile.Sync()
					}
				} else {
					b := barrierFlushIO{fileIo: p.t.io, entered: entered, release: release, once: &once}
					if failSync {
						b.err = syscall.ENOSPC
					}
					p.t.io = b
				}
				done := make(chan error, 1)
				go func() { done <- p.MarkComplete() }()
				defer func() {
					unblock.Do(func() { close(release) })
					if err := <-done; err != nil {
						t.Error("best-effort Flush rejected publication", err)
					}
					if p.t.client.opts.VerifyOnOpen && !ts.CachedComplete(0) {
						t.Error("completion not published")
					}
					s := ts.IOTimings()
					wantErrors := int64(0)
					if failSync {
						wantErrors = 1
					}
					if s.CompletionSyncCount != 1 || s.CompletionSyncActive != 0 || s.CompletionSyncErrors != wantErrors {
						t.Error(s)
					}
					if failSync && (!strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "error flushing completed piece")) {
						t.Error("Flush failure lost WARN", logs.String())
					}
				}()
				awaitWriteBarrier(t, entered)
				hint, err := pc.Get(p.pieceKey())
				if err != nil || !hint.Complete || pc.sets.Load() != 1 {
					t.Fatal("hint did not precede sync", hint, err)
				}
				if s := ts.IOTimings(); s.CompletionSyncActive != 1 || s.CompletionSyncCount != 0 {
					t.Fatal(s)
				}
				if p.t.client.opts.VerifyOnOpen && ts.CachedComplete(0) {
					t.Fatal("fresh publication did not wait for synchronous Flush return")
				}
				if p.t.lifecycle.TryLock() {
					p.t.lifecycle.Unlock()
					t.Fatal("Flush lost lifecycle join barrier")
				}
			})
		}
	}
}

func TestLegacyCompletionSetFailureAndNonpersistent(t *testing.T) {
	for _, mode := range []string{"managed", "verified", "legacy", "nonpersistent"} {
		for _, failSet := range []bool{false, true} {
			t.Run(mode+"/set-failure="+map[bool]string{false: "false", true: "true"}[failSet], func(t *testing.T) {
				ts, p, pc, _ := policyFixture(t, mode)
				injected := errors.New("hint Set failed")
				if failSet {
					pc.setError = injected
				}
				err := p.MarkComplete()
				if failSet {
					if !errors.Is(err, injected) || ts.IOTimings().CompletionSyncCount != 0 {
						t.Fatal("Set failure flushed or published", err, ts.IOTimings())
					}
					if c := p.Completion(); c.Complete {
						t.Fatal("failed Set published completion")
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					wantSync := int64(1)
					if mode == "nonpersistent" {
						wantSync = 0
					}
					if ts.IOTimings().CompletionSyncCount != wantSync {
						t.Fatal(ts.IOTimings())
					}
				}
			})
		}
	}
}

func TestLegacyCompletionCheckpointSyncStillMandatory(t *testing.T) {
	for _, stage := range []string{"payload", "directory"} {
		t.Run(stage, func(t *testing.T) {
			ts, p, _, f := policyFixture(t, "managed")
			f.syncIO = func() error { return io.ErrClosedPipe }
			if err := p.MarkComplete(); err != nil || !ts.CachedComplete(0) {
				t.Fatal("per-piece Flush was not best-effort", err)
			}
			injected := errors.New("checkpoint sync failed")
			if stage == "payload" {
				p.t.io = checkpointFlush{p.t.io, func(string) error { return injected }}
			} else {
				p.t.checkpointSyncDir = func(*os.Root, string) error { return injected }
			}
			published := false
			err := ts.WithCheckpoint(context.Background(), []byte{1}, func(FileCheckpoint) error { published = true; return nil })
			if !errors.Is(err, injected) || published {
				t.Fatal("checkpoint adopted best-effort sync policy", err, published)
			}
		})
	}
}

func TestLegacyCompletionExplicitFlushStillReturnsError(t *testing.T) {
	ts, p, _, _ := policyFixture(t, "verified")
	p.t.io = failPublicationFlush{p.t.io}
	if err := p.MarkComplete(); err != nil || !ts.CachedComplete(0) {
		t.Fatal("automatic completion Flush was not best-effort", err)
	}
	if err := p.Flush(); !errors.Is(err, os.ErrPermission) {
		t.Fatal("explicit Flush adopted best-effort policy", err)
	}
}

func TestLegacyStaleCallbackCanEnterCheckpoint(t *testing.T) {
	ts, p, _, _ := policyFixture(t, "managed")
	h := sha1.New()
	if _, err := p.WriteTo(h); err != nil {
		t.Fatal(err)
	}
	original := h.Sum(nil)
	if _, err := p.WriteAt([]byte("evil"), 0); err != nil {
		t.Fatal(err)
	}
	if ts.CachedComplete(0) {
		t.Fatal("write granted trust by itself")
	}
	if err := p.MarkComplete(); err != nil {
		t.Fatal(err)
	}
	if err := ts.WithCheckpoint(context.Background(), []byte{1}, func(c FileCheckpoint) error {
		if c.Complete[0] != 1 {
			t.Error("legacy stale-callback risk not reproduced", c.Complete)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.Reset()
	if _, err := p.WriteTo(h); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(original, h.Sum(nil)) {
		t.Fatal("fixture did not change hashed bytes")
	}
	// This is an explicit canary for the accepted integrity weakening: durable
	// checkpoint publication does NOT rehash a stale caller's completion.
}
