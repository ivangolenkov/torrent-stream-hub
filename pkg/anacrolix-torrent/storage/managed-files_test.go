package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/metainfo"
)

func managedFixture(t testing.TB, ctx context.Context, files int, guard func(string) error) (TorrentImpl, *fileTorrentImpl, *metainfo.Info) {
	t.Helper()
	info := &metainfo.Info{Name: "data", PieceLength: 4, Pieces: make([]byte, files*20)}
	for i := 0; i < files; i++ {
		info.Files = append(info.Files, metainfo.FileInfo{Length: 4, Path: []string{fmt.Sprint(i)}})
	}
	return managedInfoFixture(t, ctx, info, guard)
}

func managedInfoFixture(t testing.TB, ctx context.Context, info *metainfo.Info, guard func(string) error) (TorrentImpl, *fileTorrentImpl, *metainfo.Info) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	opts := NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, ManagedPayload: true, UsePartFiles: g.Some(false), NoCompletionRename: true, PieceCompletion: NewMapPieceCompletion(), IOContext: ctx, IOGuard: guard}
	opts.TorrentDirMaker = func(base string, _ *metainfo.Info, _ metainfo.Hash) string { return filepath.Join(base, "payload") }
	backend := NewFileOpts(opts)
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	ts, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ts.Close(); err != nil {
			t.Error(err)
		}
	})
	return ts, ts.Piece(info.Piece(0)).(*filePieceImpl).t, info
}

func verifyManagedPiece(t *testing.T, ts TorrentImpl, info *metainfo.Info, index int, want string) {
	t.Helper()
	p := ts.Piece(info.Piece(index)).(*filePieceImpl)
	if err := p.BeginVerification(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if n, err := p.WriteTo(&buf); err != nil || n != int64(len(want)) || buf.String() != want {
		t.Fatalf("hash input: n=%d data=%q err=%v", n, buf.String(), err)
	}
	if err := p.MarkComplete(); err != nil {
		t.Fatal(err)
	}
}

func TestManagedPayloadHotPathAndVerification(t *testing.T) {
	var calls atomic.Int64
	ts, fs, info := managedFixture(t, nil, 1, func(string) error { calls.Add(1); return nil })
	baseCalls := calls.Load()
	p := ts.Piece(info.Piece(0)).(*filePieceImpl)
	for i := 0; i < 100; i++ {
		if _, err := p.WriteAt([]byte("good"), 0); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load()-baseCalls != 1 {
		t.Fatalf("guard called %d times for one retained descriptor", calls.Load())
	}
	s := ts.IOTimings()
	if !s.ManagedPayload || s.FileCacheOpen != 1 || s.FileCacheMisses != 1 || s.FileCacheHits < 99 || s.WriteStat.Count != 0 || s.WriteOpenGuard.Count != 1 || s.WriteOpenDescriptor.Count != 1 {
		t.Fatal(s)
	}
	verifyManagedPiece(t, ts, info, 0, "good")
	if !ts.CachedComplete(0) {
		t.Fatal("successful verification did not grant completion")
	}
	if calls.Load()-baseCalls != 1 {
		t.Fatal("hash/read/flush reopened cached descriptor")
	}
	if err := p.BeginVerification(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.WriteAt([]byte("evil"), 0); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkComplete(); err == nil || ts.CachedComplete(0) {
		t.Fatal("same-piece mutation accepted stale hash result")
	}
	fs.managedFiles.mu.Lock()
	fd := fs.managedFiles.entries[0].file
	fs.managedFiles.mu.Unlock()
	if err := ts.Close(); err != nil {
		t.Fatal(err)
	}
	if ts.IOTimings().FileCacheOpen != 0 {
		t.Fatal("close did not clear cached descriptor gauge")
	}
	// On Windows Stat may return GetFileType/ERROR_INVALID_HANDLE after Close
	// rather than os.ErrClosed. ReadAt checks the os.File lifetime portably.
	if _, err := fd.ReadAt(make([]byte, 1), 0); !errors.Is(err, os.ErrClosed) {
		t.Fatal("backend Close retained descriptor", err)
	}
}

func TestManagedPayloadNeighborWriteAndCheckpoint(t *testing.T) {
	// Two pieces share a single file. Normal writes must not refresh/compare a
	// file-wide mtime or invalidate the neighboring successful hash.
	geometry := &metainfo.Info{Name: "data", Length: 4, PieceLength: 2, Pieces: make([]byte, 40)}
	ts, fs, _ := managedInfoFixture(t, nil, geometry, nil)
	p, q := ts.Piece(fs.info.Piece(0)).(*filePieceImpl), ts.Piece(fs.info.Piece(1)).(*filePieceImpl)
	if _, err := p.WriteAt([]byte("ok"), 0); err != nil {
		t.Fatal(err)
	}
	if err := p.BeginVerification(); err != nil {
		t.Fatal(err)
	}
	if _, err := q.WriteAt([]byte("ay"), 0); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkComplete(); err != nil {
		t.Fatal(err)
	}
	verifyManagedPiece(t, ts, fs.info, 1, "ay")
	var checkpoint FileCheckpoint
	if err := ts.WithCheckpoint(context.Background(), []byte{3}, func(c FileCheckpoint) error { checkpoint = c; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(checkpoint.Complete) != 1 || checkpoint.Complete[0] != 3 {
		t.Fatalf("internal metadata changes discarded checkpoint: %+v", checkpoint)
	}
}

func TestManagedPayloadResetReleasesPinnedIdentity(t *testing.T) {
	ts, fs, info := managedFixture(t, nil, 1, nil)
	p := ts.Piece(info.Piece(0))
	if _, err := p.WriteAt([]byte("good"), 0); err != nil {
		t.Fatal(err)
	}
	verifyManagedPiece(t, ts, info, 0, "good")
	fs.statHook = func() { t.Fatal("managed completion polling performed stat") }
	for i := 0; i < 10; i++ {
		if !ts.FreshComplete(0) {
			t.Fatal("lost cached trust")
		}
	}
	fs.statHook = nil
	if err := ts.ResetManagedPayload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ts.CachedComplete(0) {
		t.Fatal("explicit reset retained completion")
	}
	if err := os.WriteFile(fs.files[0].safeOsPath, []byte("next"), 0600); err != nil {
		t.Fatal(err)
	}
	verifyManagedPiece(t, ts, info, 0, "next")
	// Known identity changes are still rejected at the checkpoint boundary,
	// even though polling no longer promises external-mutation detection.
	if err := ts.ResetManagedPayload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(fs.files[0].safeOsPath, fs.files[0].safeOsPath+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fs.files[0].safeOsPath, []byte("good"), 0600); err != nil {
		t.Fatal(err)
	}
	verifyManagedPiece(t, ts, info, 0, "good")
}

func TestManagedPayloadPoolBoundAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, fs, _ := managedFixture(t, ctx, managedFileLimit+1, nil)
	for i := range fs.files {
		if err := os.WriteFile(fs.files[i].safeOsPath, []byte("good"), 0600); err != nil {
			if err := os.MkdirAll(filepath.Dir(fs.files[i].safeOsPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fs.files[i].safeOsPath, []byte("good"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	var leases []*managedFileLease
	for i := 0; i < managedFileLimit; i++ {
		lease, err := fs.acquireManagedFile(i, false)
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
	}
	defer func() {
		for _, l := range leases {
			l.Close()
		}
	}()
	done := make(chan error, 1)
	go func() {
		l, err := fs.acquireManagedFile(managedFileLimit, false)
		if l != nil {
			l.Close()
		}
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		fs.managedFiles.mu.Lock()
		waiting := fs.managedFiles.changed != nil // installed only by a capacity/upgrade waiter
		fs.managedFiles.mu.Unlock()
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatal("overflow acquisition did not wait for capacity", err)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("capacity waiter never became observable")
		}
		runtime.Gosched()
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("overflow acquisition did not cancel", err)
	}
	fs.managedFiles.mu.Lock()
	count := len(fs.managedFiles.entries)
	fs.managedFiles.mu.Unlock()
	if count != managedFileLimit {
		t.Fatal("descriptor limit exceeded", count)
	}
}

func TestManagedPayloadEvictionAndReacquisitionGuard(t *testing.T) {
	var calls atomic.Int64
	ts, fs, info := managedFixture(t, nil, managedFileLimit+2, func(string) error { calls.Add(1); return nil })
	baseCalls := calls.Load()
	for i := range fs.files {
		if _, err := ts.Piece(info.Piece(i)).WriteAt([]byte("good"), 0); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load()-baseCalls != int64(len(fs.files)) {
		t.Fatal("miss skipped volume validation", calls.Load())
	}
	if _, err := ts.Piece(info.Piece(0)).WriteAt([]byte("next"), 0); err != nil {
		t.Fatal(err)
	}
	if calls.Load()-baseCalls != int64(len(fs.files)+1) {
		t.Fatal("evicted descriptor reused without guard")
	}
	fs.managedFiles.mu.Lock()
	count := len(fs.managedFiles.entries)
	fs.managedFiles.mu.Unlock()
	if count != managedFileLimit {
		t.Fatal(count)
	}
}

func TestManagedPayloadResetJoinsReaders(t *testing.T) {
	ts, fs, info := managedFixture(t, nil, 1, nil)
	if _, err := ts.Piece(info.Piece(0)).WriteAt([]byte("good"), 0); err != nil {
		t.Fatal(err)
	}
	unlock := fs.lockPayloadRange(0, 4, false)
	done := make(chan error, 1)
	go func() { done <- ts.ResetManagedPayload(context.Background()) }()
	deadline := time.Now().Add(3 * time.Second)
	for fs.lifecycle.TryRLock() {
		fs.lifecycle.RUnlock()
		if time.Now().After(deadline) {
			unlock()
			<-done
			t.Fatal("reset never acquired lifecycle fence")
		}
		runtime.Gosched()
	}
	fs.managedFiles.mu.Lock()
	fd := fs.managedFiles.entries[0].file
	fs.managedFiles.mu.Unlock()
	if _, err := fd.Stat(); err != nil {
		unlock()
		<-done
		t.Fatal("reset closed active reader descriptor", err)
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := fd.ReadAt(make([]byte, 1), 0); !errors.Is(err, os.ErrClosed) {
		t.Fatal(err)
	}
}

func TestManagedPayloadDurableRevokeOnCacheHit(t *testing.T) {
	ts, fs, info := managedFixture(t, nil, 1, nil)
	p := ts.Piece(info.Piece(0))
	if _, err := p.WriteAt([]byte("good"), 0); err != nil {
		t.Fatal(err)
	}
	verifyManagedPiece(t, ts, info, 0, "good")
	injected := errors.New("durable revoke failed")
	fs.resume.hooks = &FileResumeHooks{Revoke: func([]byte) (bool, error) { return false, injected }}
	fs.resume.pending = []byte{1}
	if n, err := p.WriteAt([]byte("evil"), 0); n != 0 || !errors.Is(err, injected) {
		t.Fatal(n, err)
	}
	if !ts.CachedComplete(0) {
		t.Fatal("failed revoke altered unchanged live data")
	}
	data, err := os.ReadFile(fs.files[0].safeOsPath)
	if err != nil || string(data) != "good" {
		t.Fatal(string(data), err)
	}
	fs.resume.hooks.Revoke = func(mask []byte) (bool, error) {
		if len(mask) != 1 || mask[0] != 1 {
			t.Error("wrong revocation mask", mask)
		}
		return false, nil
	}
	if _, err := p.WriteAt([]byte("next"), 0); err != nil {
		t.Fatal(err)
	}
	if ts.CachedComplete(0) {
		t.Fatal("cached write retained stale trust")
	}
}

func TestManagedPayloadCheckpointRestore(t *testing.T) {
	ts, fs, info := managedFixture(t, nil, 1, nil)
	if _, err := ts.Piece(info.Piece(0)).WriteAt([]byte("good"), 0); err != nil {
		t.Fatal(err)
	}
	verifyManagedPiece(t, ts, info, 0, "good")
	var checkpoint FileCheckpoint
	if err := ts.WithCheckpoint(context.Background(), []byte{1}, func(c FileCheckpoint) error { checkpoint = c; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := ts.Close(); err != nil {
		t.Fatal(err)
	}
	opts := fs.client.opts
	root, err := os.OpenRoot(opts.ClientBaseDir)
	if err != nil {
		t.Fatal(err)
	}
	opts.RootFS = root
	opts.PieceCompletion = NewMapPieceCompletion()
	opts.ResumeHooks = func(context.Context, *metainfo.Info, metainfo.Hash) (*FileResumeHooks, error) {
		return &FileResumeHooks{Pending: []byte{1}, Revoke: func([]byte) (bool, error) { return false, nil }, Invalidate: func() error { return nil }}, nil
	}
	backend := NewFileOpts(opts)
	defer backend.Close()
	restored, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	token, err := restored.PrepareRestore(context.Background(), checkpoint, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.RestoreCheckpoint(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if !restored.CachedComplete(0) || !restored.CachedRestored(0) {
		t.Fatal("valid managed checkpoint not restored")
	}
	buf := make([]byte, 4)
	if n, err := restored.Piece(info.Piece(0)).ReadAt(buf, 0); err != nil || n != 4 || string(buf) != "good" {
		t.Fatal(n, err, string(buf))
	}
	if _, err := restored.Piece(info.Piece(0)).WriteAt([]byte("next"), 0); err != nil {
		t.Fatal(err)
	}
	if restored.CachedComplete(0) {
		t.Fatal("write after restored cache upgrade retained trust")
	}
}

func TestManagedPayloadResetRevokeFailureClosesLiveGate(t *testing.T) {
	ts, fs, info := managedFixture(t, nil, 1, nil)
	if _, err := ts.Piece(info.Piece(0)).WriteAt([]byte("good"), 0); err != nil {
		t.Fatal(err)
	}
	verifyManagedPiece(t, ts, info, 0, "good")
	injected := errors.New("reset revocation failed")
	fs.resume.hooks = &FileResumeHooks{Revoke: func([]byte) (bool, error) { return false, injected }}
	fs.resume.pending = []byte{1}
	if err := ts.ResetManagedPayload(context.Background()); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	if ts.CachedComplete(0) || ts.IOTimings().FileCacheOpen != 0 {
		t.Fatal("failed reset retained trust or descriptors")
	}
	if _, err := ts.Piece(info.Piece(0)).ReadAt(make([]byte, 4), 0); !errors.Is(err, ErrUnverifiedPayload) {
		t.Fatal("failed reset left read gate open", err)
	}
	if fs.resume.pending[0] != 1 {
		t.Fatal("failed reset lost durable revocation obligation")
	}
}

func TestManagedPayloadClientCloseRejectsCachedAccess(t *testing.T) {
	ts, fs, info := managedFixture(t, nil, 1, nil)
	if _, err := ts.Piece(info.Piece(0)).WriteAt([]byte("good"), 0); err != nil {
		t.Fatal(err)
	}
	if err := fs.client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Piece(info.Piece(0)).WriteAt([]byte("evil"), 0); !errors.Is(err, os.ErrClosed) {
		t.Fatal("cached writer bypassed closed parent root", err)
	}
}

func TestManagedPayloadPersistentCompletionFromReadOnlyCache(t *testing.T) {
	ts, fs, info := managedFixture(t, nil, 1, nil)
	fs.client.opts.PieceCompletion = persistentMapCompletion{NewMapPieceCompletion()}
	path := fs.files[0].safeOsPath
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("good"), 0600); err != nil {
		t.Fatal(err)
	}
	verifyManagedPiece(t, ts, info, 0, "good")
	if !ts.CachedComplete(0) || ts.IOTimings().CompletionSyncCount != 1 || ts.IOTimings().CompletionSyncErrors != 0 {
		t.Fatal(ts.IOTimings())
	}
}

func TestManagedPayloadConcurrentMissCoalescing(t *testing.T) {
	_, fs, _ := managedFixture(t, nil, 1, nil)
	path := fs.files[0].safeOsPath
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("good"), 0600); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	var once sync.Once
	var opens atomic.Int64
	r := fs.io.(rootFileIo)
	r.onEvent = func(op, _ string) {
		if op != "open" {
			return
		}
		opens.Add(1)
		once.Do(func() { close(entered); <-release })
	}
	fs.io = r
	type result struct {
		lease *managedFileLease
		err   error
	}
	done := make(chan result, 10)
	for i := 0; i < 10; i++ {
		go func() { l, err := fs.acquireManagedFile(0, false); done <- result{l, err} }()
	}
	awaitWriteBarrier(t, entered)
	unblock.Do(func() { close(release) })
	var leases []*managedFileLease
	for i := 0; i < 10; i++ {
		res := <-done
		if res.err != nil {
			t.Error(res.err)
		} else {
			leases = append(leases, res.lease)
		}
	}
	for _, l := range leases {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	}
	s := fs.ioTimings.snapshot()
	if opens.Load() != 1 || s.FileCacheOpen != 1 || s.FileCacheMisses != 1 || s.FileCacheHits != 9 {
		t.Fatal(opens.Load(), s)
	}
}

func TestManagedPayloadSharedBoundaryPublication(t *testing.T) {
	info := &metainfo.Info{Name: "bundle", PieceLength: 4, Pieces: make([]byte, 40), Files: []metainfo.FileInfo{{Length: 3, Path: []string{"a"}}, {Length: 5, Path: []string{"b"}}}}
	ts, _, _ := managedInfoFixture(t, nil, info, nil)
	p, q := ts.Piece(info.Piece(0)).(*filePieceImpl), ts.Piece(info.Piece(1))
	if _, err := p.WriteAt([]byte("good"), 0); err != nil {
		t.Fatal(err)
	}
	if err := p.BeginVerification(); err != nil {
		t.Fatal(err)
	}
	if _, err := q.WriteAt([]byte("tail"), 0); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkComplete(); err != nil {
		t.Fatal(err)
	}
	verifyManagedPiece(t, ts, info, 0, "good")
	verifyManagedPiece(t, ts, info, 1, "tail")
	if err := ts.WithCheckpoint(context.Background(), []byte{3}, func(c FileCheckpoint) error {
		if c.Complete[0] != 3 {
			t.Error("shared-boundary checkpoint lost trust", c.Complete)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestManagedPayloadKnownGuardFailureFencesCache(t *testing.T) {
	for _, atCheckpoint := range []bool{false, true} {
		name := "acquisition"
		if atCheckpoint {
			name = "checkpoint"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			injected := errors.New("known volume unavailable")
			var fail atomic.Bool
			opts := NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, ManagedPayload: true, UsePartFiles: g.Some(false), NoCompletionRename: true, PieceCompletion: NewMapPieceCompletion(),
				TorrentDirMaker: func(base string, _ *metainfo.Info, _ metainfo.Hash) string { return filepath.Join(base, "payload") },
				IOGuard: func(string) error {
					if fail.Load() {
						return injected
					}
					return nil
				}}
			backend := NewFileOpts(opts)
			defer backend.Close()
			info := &metainfo.Info{Name: "bundle", PieceLength: 4, Pieces: make([]byte, 40), Files: []metainfo.FileInfo{{Length: 4, Path: []string{"a"}}, {Length: 4, Path: []string{"b"}}}}
			ts, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := ts.Close(); !errors.Is(err, injected) {
					t.Error("guard failure lost at shutdown", err)
				}
			}()
			p := ts.Piece(info.Piece(0)).(*filePieceImpl)
			if _, err := p.WriteAt([]byte("good"), 0); err != nil {
				t.Fatal(err)
			}
			verifyManagedPiece(t, ts, info, 0, "good")
			fail.Store(true)
			if atCheckpoint {
				err = ts.WithCheckpoint(context.Background(), []byte{1}, func(FileCheckpoint) error { t.Error("guard failure published checkpoint"); return nil })
			} else {
				_, err = ts.Piece(info.Piece(1)).WriteAt([]byte("next"), 0)
			}
			if !errors.Is(err, injected) {
				t.Fatal(err)
			}
			if ts.CachedComplete(0) {
				t.Fatal("known unsafe boundary retained live trust")
			}
			if _, err := p.WriteAt([]byte("evil"), 0); !errors.Is(err, injected) {
				t.Fatal("cache hit bypassed known guard failure", err)
			}
			data, err := os.ReadFile(p.t.files[0].safeOsPath)
			if err != nil || string(data) != "good" {
				t.Fatal(string(data), err)
			}
		})
	}
}

func BenchmarkManagedPayloadChunkWrite(b *testing.B) {
	for _, managed := range []bool{false, true} {
		name := "strict"
		if managed {
			name = "managed"
		}
		b.Run(name, func(b *testing.B) {
			info := &metainfo.Info{Name: "data", Length: 32 << 10, PieceLength: 16 << 10, Pieces: make([]byte, 40)}
			var guards atomic.Int64
			ts, fs, _ := managedInfoFixture(b, nil, info, func(string) error { guards.Add(1); return nil })
			fs.client.opts.ManagedPayload = managed // before any descriptor use
			p := ts.Piece(info.Piece(0))
			data := make([]byte, 16<<10)
			if _, err := p.WriteAt(data, 0); err != nil {
				b.Fatal(err)
			}
			before := ts.IOTimings()
			guardBefore := guards.Load()
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := p.WriteAt(data, 0); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			after := ts.IOTimings()
			b.ReportMetric(float64(guards.Load()-guardBefore)/float64(b.N), "volume-guards/op")
			b.ReportMetric(float64(after.WriteStat.Count-before.WriteStat.Count)/float64(b.N), "write-stats/op")
		})
	}
}
