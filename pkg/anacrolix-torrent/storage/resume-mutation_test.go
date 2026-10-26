package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

func TestResumeMutationBeforeWriteAndCoalescing(t *testing.T) {
	dir, _, info, ts, fs := checkpointFixture(t)
	injected := errors.New("durability failure")
	var masks []byte
	fail := true
	fs.resume.hooks = &FileResumeHooks{Revoke: func(mask []byte) (bool, error) {
		masks = append(masks, mask[0])
		if fail {
			data, err := os.ReadFile(filepath.Join(dir, "payload", "film", "a"))
			if err != nil || string(data) != "abc" {
				t.Error("write preceded durable revoke", string(data), err)
			}
			return false, injected
		}
		return false, nil
	}, Invalidate: func() error { return nil }}
	fs.resume.pending = []byte{7}
	p := ts.Piece(info.Piece(0))
	if n, err := p.WriteAt([]byte("next"), 0); n != 0 || !errors.Is(err, injected) {
		t.Fatal(n, err)
	}
	if !ts.CachedComplete(0) {
		t.Fatal("failed barrier changed payload authority")
	}
	fail = false
	if _, err := p.WriteAt([]byte("next"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := p.WriteAt([]byte("next"), 0); err != nil {
		t.Fatal(err)
	}
	if ts.CachedComplete(0) || !ts.CachedComplete(1) || !ts.CachedComplete(2) {
		t.Fatal("write revoked independent pieces")
	}
	if _, err := p.ReadAt(make([]byte, 4), 0); !errors.Is(err, ErrUnverifiedPayload) {
		t.Fatal("unverified overwrite was readable", err)
	}
	if err := ts.Piece(info.Piece(1)).MarkNotComplete(); err != nil {
		t.Fatal(err)
	}
	if err := ts.Piece(info.Piece(1)).MarkNotComplete(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(masks, []byte{1, 1, 2}) {
		t.Fatal("revoke was lost or not coalesced", masks)
	}
}

func TestResumeMutationFailedPublishStillRequiresRevoke(t *testing.T) {
	_, _, info, ts, fs := checkpointFixture(t)
	var mask byte
	fs.resume.hooks = &FileResumeHooks{Revoke: func(bits []byte) (bool, error) { mask |= bits[0]; return false, nil }, Invalidate: func() error { return nil }}
	fs.resume.pending = []byte{0}
	injected := errors.New("commit acknowledgement lost")
	if err := ts.WithCheckpoint(context.Background(), []byte{5}, func(FileCheckpoint) error { return injected }); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	if _, err := ts.Piece(info.Piece(2)).WriteAt([]byte("next"), 0); err != nil {
		t.Fatal(err)
	}
	if mask != 4 {
		t.Fatal("failed publication erased possible durable trust", mask)
	}
}

func TestResumeMutationWritableRestoreRequiresHooks(t *testing.T) {
	_, backend, info, ts, _ := checkpointFixture(t)
	claim := checkpointClaims(t, ts)
	if err := ts.Close(); err != nil {
		t.Fatal(err)
	}
	var revoked byte
	backend.opts.ResumeHooks = func(context.Context, *metainfo.Info, metainfo.Hash) (*FileResumeHooks, error) {
		return &FileResumeHooks{Pending: []byte{7}, Revoke: func(mask []byte) (bool, error) { revoked |= mask[0]; return false, nil }, Invalidate: func() error { return nil }}, nil
	}
	writable, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer writable.Close()
	token, err := writable.PrepareRestore(context.Background(), claim, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := writable.RestoreCheckpoint(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if _, err := writable.Piece(info.Piece(0)).ReadAt(make([]byte, 4), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := writable.Piece(info.Piece(0)).WriteAt([]byte("next"), 0); err != nil {
		t.Fatal(err)
	}
	if revoked != 1 || writable.CachedComplete(0) || !writable.CachedComplete(1) {
		t.Fatal("wrong revoke/live trust", revoked)
	}
}

func TestResumeMutationOpenRestoreLifecycle(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			_, backend, info, ts, _ := checkpointFixture(t)
			claim := checkpointClaims(t, ts)
			if err := ts.Close(); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("restore callback failed after import")
			var retained TorrentImpl
			calls := 0
			backend.opts.ResumeHooks = func(context.Context, *metainfo.Info, metainfo.Hash) (*FileResumeHooks, error) {
				return &FileResumeHooks{Pending: []byte{7}, RestoreCandidate: true,
					Revoke: func([]byte) (bool, error) { return false, nil }, Invalidate: func() error { return nil },
					Restore: func(ctx context.Context, opened TorrentImpl) error {
						calls++
						retained = opened
						token, err := opened.PrepareRestore(ctx, claim, func(ctx context.Context) error { return ctx.Err() })
						if err == nil {
							err = opened.RestoreCheckpoint(ctx, token)
						}
						if err != nil {
							return err
						}
						if fail {
							return injected
						}
						return nil
					}}, nil
			}
			opened, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
			if calls != 1 {
				t.Fatal("restore hook not called exactly once", calls)
			}
			if fail {
				if !errors.Is(err, injected) || opened.Piece != nil || retained.CachedComplete(0) {
					t.Fatal("failed open retained imported readiness", err)
				}
			} else {
				if err != nil || !opened.CachedComplete(0) {
					t.Fatal("restore not applied before publication", err)
				}
				_ = opened.Close()
			}
		})
	}
}

func TestResumeMutationImportSuppressesOpenRestore(t *testing.T) {
	dir, backend, info, ts, _ := checkpointFixture(t)
	_ = ts.Close()
	base := filepath.Join(dir, "payload", "film")
	if err := os.Rename(filepath.Join(base, "a"), filepath.Join(base, "a.part")); err != nil {
		t.Fatal(err)
	}
	backend.opts.ImportPartFiles = true
	invalidated := 0
	backend.opts.ResumeHooks = func(context.Context, *metainfo.Info, metainfo.Hash) (*FileResumeHooks, error) {
		return &FileResumeHooks{Pending: []byte{7}, RestoreCandidate: true,
			Revoke:     func([]byte) (bool, error) { return false, nil },
			Invalidate: func() error { invalidated++; return nil },
			Restore:    func(context.Context, TorrentImpl) error { t.Error("restore ran after namespace import"); return nil }}, nil
	}
	opened, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if invalidated != 1 || opened.CachedComplete(0) {
		t.Fatal("namespace import preserved trust", invalidated)
	}
}

func TestResumeMutationNamespaceBarrier(t *testing.T) {
	for _, mutation := range []string{"part-import", "empty-create"} {
		t.Run(mutation, func(t *testing.T) {
			dir, backend, info, ts, _ := checkpointFixture(t)
			if err := ts.Close(); err != nil {
				t.Fatal(err)
			}
			base := filepath.Join(dir, "payload", "film")
			if mutation == "part-import" {
				if err := os.Rename(filepath.Join(base, "a"), filepath.Join(base, "a.part")); err != nil {
					t.Fatal(err)
				}
				backend.opts.ImportPartFiles = true
			} else {
				if err := os.Remove(filepath.Join(base, "empty")); err != nil {
					t.Fatal(err)
				}
			}
			injected := errors.New("namespace revoke failed")
			fail := true
			calls := 0
			backend.opts.ResumeHooks = func(context.Context, *metainfo.Info, metainfo.Hash) (*FileResumeHooks, error) {
				return &FileResumeHooks{Pending: []byte{7}, Revoke: func([]byte) (bool, error) { return false, nil }, Invalidate: func() error {
					calls++
					if fail {
						return injected
					}
					return nil
				}}, nil
			}
			if _, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{}); !errors.Is(err, injected) {
				t.Fatal(err)
			}
			missing := "empty"
			if mutation == "part-import" {
				missing = "a"
			}
			if _, err := os.Stat(filepath.Join(base, missing)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("namespace effect crossed failed barrier", err)
			}
			fail = false
			opened, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			if calls != 2 {
				t.Fatal("barrier skipped", calls)
			}
			if _, err := os.Stat(filepath.Join(base, missing)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResumeMutationReadLeaseJoinsBeforeOverwrite(t *testing.T) {
	_, _, info, ts, fs := checkpointFixture(t)
	fs.resume.pending = []byte{7}
	var released atomic.Bool
	fs.resume.hooks = &FileResumeHooks{Revoke: func(mask []byte) (bool, error) {
		if mask[0]&1 != 0 && !released.Load() {
			t.Error("write revoked before existing read lease joined")
		}
		return false, nil
	}, Invalidate: func() error { return nil }}
	// Model an actual syscall lease on b, shared by v1 boundary pieces 0/1.
	fs.files[1].payloadMu.RLock()
	var once sync.Once
	unblock := func() { once.Do(func() { released.Store(true); fs.files[1].payloadMu.RUnlock() }) }
	defer unblock()
	wrote := make(chan error, 1)
	go func() { _, err := ts.Piece(info.Piece(0)).WriteAt([]byte("next"), 0); wrote <- err }()
	// An unrelated file must not wait on b's read lease.
	if _, err := ts.Piece(info.Piece(2)).WriteAt([]byte("next"), 0); err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("writer did not join read lease")
	}
}

func TestResumeMutationEvidenceRevokesSharedBoundary(t *testing.T) {
	dir, _, _, ts, fs := checkpointFixture(t)
	var revoked byte
	fs.resume.pending = []byte{7}
	fs.resume.hooks = &FileResumeHooks{Revoke: func(mask []byte) (bool, error) { revoked |= mask[0]; return false, nil }, Invalidate: func() error { return nil }}
	if err := os.Remove(filepath.Join(dir, "payload", "film", "b")); err != nil {
		t.Fatal(err)
	}
	if ts.FreshComplete(0) || ts.CachedComplete(1) || !ts.CachedComplete(2) || revoked != 3 {
		t.Fatal("missing boundary file kept persisted/live trust", revoked)
	}
}

func TestResumeMutationDiscardedOwnerClearsPendingCache(t *testing.T) {
	_, _, info, ts, fs := checkpointFixture(t)
	calls := 0
	fs.resume.pending = []byte{7}
	fs.resume.hooks = &FileResumeHooks{Revoke: func([]byte) (bool, error) { calls++; return true, nil }, Invalidate: func() error { return nil }}
	for _, index := range []int{0, 1, 2} {
		if _, err := ts.Piece(info.Piece(index)).WriteAt([]byte("next"), 0); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatal("discarded generation caused per-piece safety transactions", calls)
	}
}

func TestResumeMutationBudgetIncludesPendingMasks(t *testing.T) {
	c := FileCheckpoint{Complete: make([]byte, checkpointMaxPieces/8), Files: make([]CheckpointFile, 1)}
	for i := 0; i < 14336; i++ {
		c.Complete[i] = 255
	} // 28 MiB piece references + 4 MiB masks + file overhead
	if checkpointImportBudget(c) {
		t.Fatal("restore ignored writable pending/revoke mask memory")
	}
	clear(c.Complete[:32])
	if !checkpointImportBudget(c) {
		t.Fatal("bounded candidate unnecessarily rejected")
	}
}

func TestResumeMutationCreateRevokesImplicitFileExtent(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "truncated", true: "missing"}[missing], func(t *testing.T) {
			dir, _, info, ts, fs := checkpointFixture(t)
			path := filepath.Join(dir, "payload", "film", "b")
			if missing {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Truncate(path, 1); err != nil {
					t.Fatal(err)
				}
			}
			var revoked byte
			fail := true
			fs.resume.pending = []byte{7}
			fs.resume.hooks = &FileResumeHooks{Revoke: func(mask []byte) (bool, error) {
				revoked |= mask[0]
				if mask[0]&1 != 0 && fail {
					return false, errors.New("implicit extent revoke failed")
				}
				return false, nil
			}, Invalidate: func() error { return nil }}
			p := ts.Piece(info.Piece(1)) // writes b[1:5], creating an implicit prefix if absent
			if n, err := p.WriteAt([]byte("next"), 0); n != 0 || err == nil {
				t.Fatal("create/extension crossed failed file barrier", n, err)
			}
			st, err := os.Stat(path)
			if missing {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatal("O_CREATE preceded revoke", err)
				}
			} else {
				if err != nil || st.Size() != 1 {
					t.Fatal("extension preceded revoke", st, err)
				}
			}
			fail = false
			if _, err := p.WriteAt([]byte("next"), 0); err != nil {
				t.Fatal(err)
			}
			if revoked != 3 || ts.CachedComplete(0) || ts.CachedComplete(1) || !ts.CachedComplete(2) {
				t.Fatal("implicit extent preserved boundary trust or revoked independent file", revoked)
			}
		})
	}
}

func TestResumeMutationReadErrorClosesLiveGateOnRevokeFailure(t *testing.T) {
	dir, _, info, ts, fs := checkpointFixture(t)
	fs.resume.pending = []byte{7}
	injected := errors.New("revoke unavailable")
	fs.resume.hooks = &FileResumeHooks{Revoke: func([]byte) (bool, error) { return false, injected }, Invalidate: func() error { return nil }}
	if err := os.Remove(filepath.Join(dir, "payload", "film", "b")); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Piece(info.Piece(0)).ReadAt(make([]byte, 4), 0); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	if ts.CachedComplete(0) || ts.CachedComplete(1) || !ts.CachedComplete(2) {
		t.Fatal("known read error remained live after failed durable revoke")
	}
}

func TestResumeMutationCheckpointUsesIndependentShutdownContext(t *testing.T) {
	_, _, info, ts, fs := checkpointFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rooted := fs.io.(rootFileIo)
	rooted.ctx = ctx
	fs.io = rooted
	published := false
	if err := ts.WithCheckpoint(context.Background(), []byte{7}, func(FileCheckpoint) error { published = true; return nil }); err != nil || !published {
		t.Fatal("checkpoint inherited cancelled runtime I/O context", published, err)
	}
	if n, err := ts.Piece(info.Piece(0)).WriteAt([]byte("next"), 0); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal("checkpoint context override reopened cancelled writer", n, err)
	}
}

func TestResumeMutationClosedBackendCannotReadReusedRoot(t *testing.T) {
	dir, _, info, ts, _ := checkpointFixture(t)
	p := ts.Piece(info.Piece(0))
	if err := ts.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "payload", "film", "a"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if n, err := p.ReadAt(make([]byte, 4), 0); n != 0 || !errors.Is(err, os.ErrClosed) {
		t.Fatal("closed attachment read replacement payload", n, err)
	}
	if n, err := p.(interface {
		WriteTo(io.Writer) (int64, error)
	}).WriteTo(io.Discard); n != 0 || !errors.Is(err, os.ErrClosed) {
		t.Fatal("closed hash reader read replacement payload", n, err)
	}
	if ts.CachedComplete(0) || ts.CachedEmptyFile(3) {
		t.Fatal("closed attachment retained live readiness")
	}
}

func TestResumeMutationCloseJoinsEvidenceRevocation(t *testing.T) {
	dir, _, _, ts, fs := checkpointFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	fs.resume.pending = []byte{7}
	fs.resume.hooks = &FileResumeHooks{Revoke: func([]byte) (bool, error) { close(entered); <-release; return false, nil }, Invalidate: func() error { return nil }}
	if err := os.Remove(filepath.Join(dir, "payload", "film", "b")); err != nil {
		t.Fatal(err)
	}
	refreshed := make(chan struct{})
	go func() { ts.FreshComplete(0); close(refreshed) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("evidence callback did not start")
	}
	if fs.files[1].payloadMu.TryLock() {
		fs.files[1].payloadMu.Unlock()
		t.Fatal("evidence callback did not retain its file lease")
	}
	closed := make(chan struct{})
	go func() { _ = ts.Close(); close(closed) }()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for !fs.readClosed.Load() {
		select {
		case <-tick.C:
		case <-timeout.C:
			t.Fatal("close did not revoke read admission")
		}
	}
	select {
	case <-closed:
		t.Fatal("close released owner before evidence SQL callback joined")
	default:
	}
	unblock()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("close did not join evidence callback")
	}
	select {
	case <-refreshed:
	case <-time.After(3 * time.Second):
		t.Fatal("evidence reader did not finish")
	}
}
