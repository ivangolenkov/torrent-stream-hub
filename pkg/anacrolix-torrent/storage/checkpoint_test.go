package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/metainfo"
)

func checkpointFixture(t *testing.T) (string, *fileClientImpl, *metainfo.Info, TorrentImpl, *fileTorrentImpl) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	info := &metainfo.Info{Name: "film", PieceLength: 4, Files: []metainfo.FileInfo{{Path: []string{"a"}, Length: 3}, {Path: []string{"b"}, Length: 5}, {Path: []string{"c"}, Length: 4}, {Path: []string{"empty"}, Length: 0}}, Pieces: make([]byte, 60)}
	opts := NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, NoCompletionRename: true, UsePartFiles: g.Some(false), PieceCompletion: NewMapPieceCompletion(),
		TorrentPathResolver: func(base string, info *metainfo.Info, _ metainfo.Hash) (string, []string, error) {
			paths := make([]string, 0, len(info.Files))
			for _, f := range info.UpvertedFiles() {
				paths = append(paths, filepath.Join(f.Path...))
			}
			return filepath.Join(base, "payload", "film"), paths, nil
		}}
	backend := NewFileOpts(opts).(*fileClientImpl)
	t.Cleanup(func() { _ = backend.Close() })
	base := filepath.Join(dir, "payload", "film")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"a": "abc", "b": "defgh", "c": "ijkl"} {
		if err := os.WriteFile(filepath.Join(base, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ts, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ts.Close() })
	for i := 0; i < 3; i++ {
		p := ts.Piece(info.Piece(i)).(*filePieceImpl)
		// As in verified_file_test, the hash caller is the authority for bytes;
		// these backend unit tests exercise signature/flush protocol, not SHA-1.
		if err := p.BeginVerification(); err != nil {
			t.Fatal(err)
		}
		if err := p.MarkComplete(); err != nil {
			t.Fatal(err)
		}
	}
	return dir, backend, info, ts, ts.Piece(info.Piece(0)).(*filePieceImpl).t
}

func checkpointClaims(t *testing.T, ts TorrentImpl) FileCheckpoint {
	t.Helper()
	var c FileCheckpoint
	if err := ts.WithCheckpoint(context.Background(), []byte{7}, func(v FileCheckpoint) error { c = v; return nil }); err != nil {
		t.Fatal(err)
	}
	return c
}

func readonlyCheckpointOpen(t *testing.T, backend *fileClientImpl, info *metainfo.Info) TorrentImpl {
	t.Helper()
	opts := backend.opts
	var err error
	opts.RootFS, err = os.OpenRoot(opts.ClientBaseDir)
	if err != nil {
		t.Fatal(err)
	}
	opts.ReadOnly, opts.ImportPartFiles, opts.NoCompletionRename = true, false, true
	opts.PieceCompletion = NewMapPieceCompletion()
	b := NewFileOpts(opts)
	t.Cleanup(func() { _ = b.Close() })
	ts, err := b.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ts.Close() })
	return ts
}

type checkpointFlush struct {
	fileIo
	flushFn func(string) error
}

func TestCheckpointObservationBoundariesFollowSuccessfulHelpers(t *testing.T) {
	_, backend, _, ts, fs := checkpointFixture(t)
	var events []string
	backend.opts.IOEvent = func(op, path string) {
		if strings.HasPrefix(op, "checkpoint-") {
			events = append(events, op+":"+filepath.ToSlash(path))
		}
	}
	if err := ts.WithCheckpoint(context.Background(), []byte{7}, func(FileCheckpoint) error { events = append(events, "publish"); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(events) != 17 || events[0] != "checkpoint-snapshot-before:" || events[1] != "checkpoint-snapshot-after:" || events[len(events)-1] != "publish" {
		t.Fatal("incomplete observation sequence", events)
	}
	for i := 2; i < len(events)-1; i += 2 {
		if strings.Replace(events[i], "-before:", "-after:", 1) != events[i+1] {
			t.Fatal("after event preceded successful helper", events)
		}
	}
	original := fs.io
	fs.io = checkpointFlush{original, func(string) error { return os.ErrPermission }}
	events = nil
	if err := ts.WithCheckpoint(context.Background(), []byte{7}, func(FileCheckpoint) error { t.Fatal("published after failed sync"); return nil }); !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
	if len(events) != 3 || events[2] != "checkpoint-payload-sync-before:payload/film/a" {
		t.Fatal("failed helper emitted after event", events)
	}
}

func TestCheckpointDirtyAcknowledgesOnlyCapturedRevision(t *testing.T) {
	_, _, _, ts, fs := checkpointFixture(t)
	before, dirty := ts.CheckpointState()
	if !dirty {
		t.Fatal("verified owner not dirty")
	}
	injected := errors.New("publish failed")
	if err := ts.WithCheckpoint(context.Background(), []byte{7}, func(FileCheckpoint) error { return injected }); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	if revision, dirty := ts.CheckpointState(); revision != before || !dirty {
		t.Fatal("failed seal acknowledged revision", revision, dirty)
	}
	checkpointClaims(t, ts)
	if _, dirty := ts.CheckpointState(); dirty {
		t.Fatal("successful seal left unchanged owner dirty")
	}
	if err := ts.WithCheckpoint(context.Background(), []byte{7}, func(FileCheckpoint) error {
		// Readers may withdraw evidence while publication is in flight. That
		// newer revision must not be acknowledged by this older capture.
		return fs.revokeResumeFile(2)
	}); err != nil {
		t.Fatal(err)
	}
	if revision, dirty := ts.CheckpointState(); revision <= before || !dirty {
		t.Fatal("seal acknowledged a later evidence withdrawal", revision, dirty)
	}
	if err := ts.Close(); err != nil {
		t.Fatal(err)
	}
	if _, dirty := ts.CheckpointState(); dirty {
		t.Fatal("closed attachment was scheduled")
	}
}

func TestCheckpointRestoreChecksEpochAfterOwnerCallback(t *testing.T) {
	_, backend, info, ts, _ := checkpointFixture(t)
	claim := checkpointClaims(t, ts)
	ro := readonlyCheckpointOpen(t, backend, info)
	fs := ro.Piece(info.Piece(0)).(*filePieceImpl).t
	calls := 0
	token, err := ro.PrepareRestore(context.Background(), claim, func(context.Context) error {
		calls++
		if calls == 3 {
			// Simulate a read-side evidence withdrawal after rooted validation.
			return fs.revokeResumeFile(0)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ro.RestoreCheckpoint(context.Background(), token); !errors.Is(err, ErrCheckpointChanged) {
		t.Fatal("late evidence change admitted", err)
	}
	if ro.CachedComplete(0) {
		t.Fatal("late token granted readiness")
	}
}

func TestCheckpointCancelledRuntimePreservesEvidence(t *testing.T) {
	for _, cancelErr := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cancelErr.Error(), func(t *testing.T) {
			_, _, info, ts, fs := checkpointFixture(t)
			var revoked atomic.Int64
			fs.resume.hooks = &FileResumeHooks{Revoke: func([]byte) (bool, error) { revoked.Add(1); return false, nil }}
			fs.resume.pending = []byte{7}
			checkpointClaims(t, ts)
			// Guard failures occur before actual filesystem reads/stat.
			rooted := fs.io.(rootFileIo)
			rooted.guard = func(string) error { return cancelErr }
			fs.io = rooted
			fs.refreshFile(0)
			fs.refreshEmptyFile(3)
			_, err := ts.Piece(info.Piece(0)).ReadAt(make([]byte, 4), 0)
			if !errors.Is(err, cancelErr) {
				t.Fatal(err)
			}
			if !ts.CachedComplete(0) || !ts.CachedEmptyFile(3) || revoked.Load() != 0 {
				t.Fatal("runtime cancellation treated as data corruption", revoked.Load())
			}
			if _, dirty := ts.CheckpointState(); dirty {
				t.Fatal("cancelled probe dirtied checkpoint")
			}
		})
	}
}

func (f checkpointFlush) flush(name string, _, _ int64) error { return f.flushFn(name) }

func TestCheckpointFlushPublishAndReadonlyRestore(t *testing.T) {
	_, backend, info, ts, fs := checkpointFixture(t)
	var reads atomic.Int64
	backend.opts.IORead = func(string, int64) { reads.Add(1) }
	var claim FileCheckpoint
	if err := ts.WithCheckpoint(context.Background(), []byte{5}, func(c FileCheckpoint) error {
		claim = c
		if !bytes.Equal(c.Complete, []byte{5}) {
			t.Fatal("eligibility expanded", c.Complete)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if claim.Root != filepath.Join("payload", "film") || len(claim.Files) != 4 || claim.Files[1].Identity == "" || claim.Files[1].Offset != 3 {
		t.Fatal(claim)
	}
	// A writable backend cannot import trust before mutation hooks exist.
	if _, err := ts.PrepareRestore(context.Background(), claim, func(context.Context) error { return nil }); !errors.Is(err, ErrCheckpointUnsupported) {
		t.Fatal("writable import admitted", err)
	}
	if err := ts.Close(); err != nil {
		t.Fatal(err)
	}
	ro := readonlyCheckpointOpen(t, backend, info)
	if ro.CachedComplete(0) {
		t.Fatal("reopen trusted hints")
	}
	token, err := ro.PrepareRestore(context.Background(), claim, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := ro.RestoreCheckpoint(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if !ro.CachedComplete(0) || ro.CachedComplete(1) || !ro.CachedComplete(2) {
		t.Fatal("restored wrong subset")
	}
	if err := ro.RestoreCheckpoint(context.Background(), token); !errors.Is(err, ErrCheckpointChanged) {
		t.Fatal("replayed token", err)
	}
	if reads.Load() != 0 {
		t.Fatal("restore hashed payload", reads.Load())
	}
	buf := make([]byte, 4)
	if _, err := ro.Piece(info.Piece(0)).ReadAt(buf, 0); err != nil || string(buf) != "abcd" {
		t.Fatal(string(buf), err)
	}
	if _, err := ro.Piece(info.Piece(0)).WriteAt([]byte("evil"), 0); !errors.Is(err, os.ErrPermission) {
		t.Fatal("readonly restored storage wrote", err)
	}
	_ = fs
}

func TestCheckpointFailuresNeverPublish(t *testing.T) {
	for _, failure := range []string{"file-flush", "directory-flush", "file-enospc", "directory-enospc", "access-denied", "deadline", "changed", "cancel", "publish"} {
		t.Run(failure, func(t *testing.T) {
			dir, _, _, ts, fs := checkpointFixture(t)
			original := fs.io
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			injected := errors.New("injected boundary failure")
			published := false
			switch failure {
			case "file-enospc", "access-denied":
				if failure == "file-enospc" {
					injected = syscall.ENOSPC
				} else {
					injected = os.ErrPermission
				}
				fs.io = checkpointFlush{original, func(string) error { return injected }}
			case "directory-enospc":
				injected = syscall.ENOSPC
				fs.checkpointSyncDir = func(*os.Root, string) error { return injected }
			case "deadline":
				injected = context.DeadlineExceeded
				fs.io = checkpointFlush{original, func(string) error { return injected }}
			case "file-flush":
				fs.io = checkpointFlush{original, func(string) error { return injected }}
			case "directory-flush":
				fs.checkpointSyncDir = func(*os.Root, string) error { return injected }
			case "changed", "cancel":
				changed := false
				fs.io = checkpointFlush{original, func(name string) error {
					if err := original.flush(name, 0, 0); err != nil {
						return err
					}
					if !changed {
						changed = true
						if failure == "cancel" {
							cancel()
						} else {
							path := filepath.Join(dir, "payload", "film", "b")
							if err := os.Chtimes(path, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
								return err
							}
						}
					}
					return nil
				}}
			}
			err := ts.WithCheckpoint(ctx, []byte{7}, func(FileCheckpoint) error { published = true; return injected })
			if err == nil || published != (failure == "publish") {
				t.Fatal("bad publication boundary", published, err)
			}
			if failure == "file-enospc" || failure == "directory-enospc" || failure == "access-denied" || failure == "deadline" {
				if !errors.Is(err, injected) {
					t.Fatal("storage diagnosis lost", err)
				}
			}
			fs.io, fs.checkpointSyncDir = original, nil
			if _, err := ts.Piece(fs.info.Piece(2)).WriteAt([]byte("next"), 0); err != nil {
				t.Fatal("admission not released", err)
			}
		})
	}
}

func TestCheckpointWriteJoinAndCancellation(t *testing.T) {
	_, _, info, ts, fs := checkpointFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	finish := make(chan error, 1)
	go func() {
		finish <- ts.WithCheckpoint(context.Background(), []byte{7}, func(FileCheckpoint) error { close(entered); <-release; return nil })
	}()
	<-entered
	if fs.lifecycle.TryRLock() {
		fs.lifecycle.RUnlock()
		close(release)
		t.Fatal("publication did not hold exclusive mutation fence")
	}
	wrote := make(chan error, 1)
	go func() { _, err := ts.Piece(info.Piece(0)).WriteAt([]byte("next"), 0); wrote <- err }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := fs.withCheckpoint(ctx, []byte{7}, func(FileCheckpoint) error { return nil }); !errors.Is(err, context.Canceled) {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := <-finish; err != nil {
		t.Fatal(err)
	}
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	if ts.CachedComplete(0) {
		t.Fatal("write retained old freshness")
	}
}

type checkpointBlockedWriter struct {
	fileIo
	entered chan struct{}
	release chan struct{}
}

func (f checkpointBlockedWriter) openForWrite(name string, size int64) (fileWriter, error) {
	close(f.entered)
	<-f.release
	return f.fileIo.openForWrite(name, size)
}

func TestCheckpointJoinsBlockedWriter(t *testing.T) {
	_, _, info, ts, fs := checkpointFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	fs.io = checkpointBlockedWriter{fs.io, entered, release}
	wrote := make(chan error, 1)
	// Piece 2 lives in one file, so the barrier is entered exactly once.
	go func() { _, err := ts.Piece(info.Piece(2)).WriteAt([]byte("next"), 0); wrote <- err }()
	<-entered
	if fs.lifecycle.TryLock() {
		fs.lifecycle.Unlock()
		close(release)
		<-wrote
		t.Fatal("writer did not retain mutation admission through syscall")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := ts.WithCheckpoint(ctx, []byte{7}, func(FileCheckpoint) error { t.Error("published before writer joined"); return nil })
	cancel()
	// Cancellation must not release the writer's resource ownership.
	stillOwned := !fs.lifecycle.TryLock()
	if !stillOwned {
		fs.lifecycle.Unlock()
	}
	close(release)
	writeErr := <-wrote
	if !errors.Is(err, context.DeadlineExceeded) || !stillOwned || writeErr != nil {
		t.Fatal(err, stillOwned, writeErr)
	}
	if err := ts.WithCheckpoint(context.Background(), []byte{7}, func(c FileCheckpoint) error {
		if c.Complete[0] != 3 {
			t.Error("unverified write retained freshness", c.Complete)
		}
		return nil
	}); err != nil {
		t.Fatal("admission not released after writer joined", err)
	}
}

func TestCheckpointRestoreRevocationAndEvidence(t *testing.T) {
	for _, change := range []string{"missing-boundary", "mtime-boundary", "missing-empty", "owner", "owner-during-reprobe", "cancel", "verification", "other-open", "after-prepare"} {
		t.Run(change, func(t *testing.T) {
			dir, backend, info, ts, _ := checkpointFixture(t)
			claim := checkpointClaims(t, ts)
			if err := ts.Close(); err != nil {
				t.Fatal(err)
			}
			base := filepath.Join(dir, "payload", "film")
			if change == "missing-boundary" {
				if err := os.Remove(filepath.Join(base, "b")); err != nil {
					t.Fatal(err)
				}
			}
			if change == "missing-empty" {
				if err := os.Remove(filepath.Join(base, "empty")); err != nil {
					t.Fatal(err)
				}
			}
			if change == "mtime-boundary" {
				if err := os.Chtimes(filepath.Join(base, "b"), time.Unix(1, 0), time.Unix(1, 0)); err != nil {
					t.Fatal(err)
				}
			}
			ownerValid := true
			reprobe := false
			backend.opts.IOEvent = func(string, string) {
				if change == "owner-during-reprobe" && reprobe {
					ownerValid = false
				}
			}
			ro := readonlyCheckpointOpen(t, backend, info)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			token, err := ro.PrepareRestore(ctx, claim, func(context.Context) error {
				if !ownerValid {
					return ErrCheckpointChanged
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			reprobe = true
			switch change {
			case "owner":
				ownerValid = false
			case "cancel":
				cancel()
			case "verification":
				if err := ro.Piece(info.Piece(0)).(*filePieceImpl).BeginVerification(); err != nil {
					t.Fatal(err)
				}
			case "after-prepare":
				if err := os.Chtimes(filepath.Join(base, "c"), time.Unix(1, 0), time.Unix(1, 0)); err != nil {
					t.Fatal(err)
				}
			case "other-open":
				other := readonlyCheckpointOpen(t, backend, info)
				if err := other.RestoreCheckpoint(context.Background(), token); !errors.Is(err, ErrCheckpointChanged) {
					t.Fatal("token crossed open generation", err)
				}
			}
			err = ro.RestoreCheckpoint(context.Background(), token)
			invalid := change == "owner" || change == "owner-during-reprobe" || change == "cancel" || change == "verification" || change == "after-prepare"
			if invalid {
				if err == nil {
					t.Fatal("stale token restored")
				}
				for i := 0; i < 3; i++ {
					if ro.CachedComplete(i) {
						t.Fatal("partial trust after failed import")
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := byte(7)
			if change == "missing-boundary" || change == "mtime-boundary" {
				want = 4
			}
			for i := 0; i < 3; i++ {
				if ro.CachedComplete(i) != checkpointBit([]byte{want}, i) {
					t.Fatal("wrong file-dependent subset", i)
				}
			}
			if change == "missing-empty" {
				if _, err := os.Stat(filepath.Join(base, "empty")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("restore created empty file", err)
				}
			}
		})
	}
}

var _ io.ReaderAt = (*filePieceImpl)(nil)
