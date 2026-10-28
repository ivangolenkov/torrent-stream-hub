package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

type managedTestFile struct {
	managedPayloadFile
	write  func([]byte, int64) (int, error)
	read   func([]byte, int64) (int, error)
	syncIO func() error
	closes atomic.Int64
}

func (f *managedTestFile) WriteAt(p []byte, off int64) (int, error) {
	if f.write != nil {
		return f.write(p, off)
	}
	return f.managedPayloadFile.WriteAt(p, off)
}
func (f *managedTestFile) ReadAt(p []byte, off int64) (int, error) {
	if f.read != nil {
		return f.read(p, off)
	}
	return f.managedPayloadFile.ReadAt(p, off)
}
func (f *managedTestFile) Close() error {
	f.closes.Add(1)
	return f.managedPayloadFile.Close()
}

func (f *managedTestFile) Sync() error {
	if f.syncIO != nil {
		return f.syncIO()
	}
	return f.managedPayloadFile.Sync()
}

func managedRangeFixture(t *testing.T, boundary bool) (TorrentImpl, *fileTorrentImpl, *metainfo.Info, *managedTestFile) {
	t.Helper()
	info := &metainfo.Info{Name: "data", Length: 8, PieceLength: 4, Pieces: make([]byte, 40)}
	if boundary {
		info.Name, info.Length = "bundle", 0
		info.Files = []metainfo.FileInfo{{Length: 3, Path: []string{"a"}}, {Length: 5, Path: []string{"b"}}}
	}
	ts, fs, _ := managedInfoFixture(t, nil, info, nil)
	if _, err := (fileTorrentImplIO{fs}).WriteAt([]byte("goodtail"), 0); err != nil {
		t.Fatal(err)
	}
	fs.managedFiles.mu.Lock()
	e := fs.managedFiles.entries[len(fs.files)-1]
	f := &managedTestFile{managedPayloadFile: e.file}
	e.file = f
	fs.managedFiles.mu.Unlock()
	return ts, fs, info, f
}

func awaitManagedResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("independent managed operation did not finish")
		return nil
	}
}

func awaitManagedRanges(t *testing.T, fs *fileTorrentImpl, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		fs.managedRanges.mu.Lock()
		n := 0
		for r := fs.managedRanges.head; r != nil; r = r.next {
			n++
		}
		fs.managedRanges.mu.Unlock()
		if n == count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("managed range requests: got %d want %d", n, count)
		}
		runtime.Gosched()
	}
}

func startManagedWrite(ts TorrentImpl, info *metainfo.Info, index int, data string) <-chan error {
	done := make(chan error, 1)
	go func() { _, err := ts.Piece(info.Piece(index)).WriteAt([]byte(data), 0); done <- err }()
	return done
}

func TestManagedPayloadIndependentWrites(t *testing.T) {
	for _, boundary := range []bool{false, true} {
		name := "single-file"
		if boundary {
			name = "shared-boundary"
		}
		t.Run(name, func(t *testing.T) {
			ts, fs, info, f := managedRangeFixture(t, boundary)
			p := ts.Piece(info.Piece(0)).(*filePieceImpl)
			if err := p.BeginVerification(); err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once, unblock sync.Once
			defer unblock.Do(func() { close(release) })
			f.write = func(b []byte, off int64) (int, error) {
				first := false
				once.Do(func() { first = true; close(entered) })
				if first {
					<-release
				}
				return f.managedPayloadFile.WriteAt(b, off)
			}
			done := startManagedWrite(ts, info, 0, "GOOD")
			defer func() {
				unblock.Do(func() { close(release) })
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			awaitWriteBarrier(t, entered)
			// This must finish while the first real borrowed handle's WriteAt is
			// still stopped, including two pieces sharing a boundary file.
			if err := awaitManagedResult(t, startManagedWrite(ts, info, 1, "TAIL")); err != nil {
				t.Fatal(err)
			}
			verifyManagedPiece(t, ts, info, 1, "TAIL")
			if s := ts.IOTimings(); s.WritePayload.Active != 1 || s.WriteLeaseWaitActive != 0 {
				t.Fatal(s)
			}
			if f.closes.Load() != 0 {
				t.Fatal("borrowed handle retired during independent I/O")
			}
			unblock.Do(func() { close(release) })
			// Join the first write before testing its erased snapshot.
			// The deferred join consumes done, so observe its range release here.
			awaitManagedRanges(t, fs, 0)
			if err := p.MarkComplete(); err == nil || ts.CachedComplete(0) {
				t.Fatal("write resurrected pre-mutation hash")
			}
			verifyManagedPiece(t, ts, info, 0, "GOOD")
			if !ts.CachedComplete(1) {
				t.Fatal("neighbor publication invalidated")
			}
			if err := ts.WithCheckpoint(context.Background(), []byte{3}, func(c FileCheckpoint) error {
				if c.Complete[0] != 3 {
					t.Error("checkpoint lost independent pieces", c.Complete)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestManagedPayloadOverlappingWriteWaits(t *testing.T) {
	ts, fs, info, f := managedRangeFixture(t, false)
	entered, release := make(chan struct{}), make(chan struct{})
	var once, unblock sync.Once
	defer unblock.Do(func() { close(release) })
	f.write = func(b []byte, off int64) (int, error) {
		first := false
		once.Do(func() { first = true; close(entered) })
		if first {
			<-release
		}
		return f.managedPayloadFile.WriteAt(b, off)
	}
	first := startManagedWrite(ts, info, 0, "GOOD")
	defer func() { unblock.Do(func() { close(release) }); <-first }()
	awaitWriteBarrier(t, entered)
	second := startManagedWrite(ts, info, 0, "last")
	defer func() { unblock.Do(func() { close(release) }); <-second }()
	awaitManagedRanges(t, fs, 2)
	if len(second) != 0 {
		t.Fatal("overlapping write escaped exclusion")
	}
	if s := ts.IOTimings(); s.WritePayload.Active != 1 || s.WriteLeaseWaitActive != 1 {
		t.Fatal(s)
	}
	if err := awaitManagedResult(t, startManagedWrite(ts, info, 1, "TAIL")); err != nil {
		t.Fatal(err)
	}
	unblock.Do(func() { close(release) })
	awaitManagedRanges(t, fs, 0)
	verifyManagedPiece(t, ts, info, 0, "last")
}

func TestManagedPayloadIndependentChunksOfSamePiece(t *testing.T) {
	info := &metainfo.Info{Name: "data", Length: 8, PieceLength: 8, Pieces: make([]byte, 20)}
	ts, fs, _ := managedInfoFixture(t, nil, info, nil)
	p := ts.Piece(info.Piece(0)).(*filePieceImpl)
	if _, err := p.WriteAt([]byte("goodtail"), 0); err != nil {
		t.Fatal(err)
	}
	fs.managedFiles.mu.Lock()
	e := fs.managedFiles.entries[0]
	f := &managedTestFile{managedPayloadFile: e.file}
	e.file = f
	fs.managedFiles.mu.Unlock()
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	f.write = func(b []byte, off int64) (int, error) {
		if off == 0 {
			close(entered)
			<-release
		}
		return f.managedPayloadFile.WriteAt(b, off)
	}
	first := startManagedWrite(ts, info, 0, "GOOD")
	defer func() {
		unblock.Do(func() { close(release) })
		if err := <-first; err != nil {
			t.Error(err)
		}
	}()
	awaitWriteBarrier(t, entered)
	second := make(chan error, 1)
	go func() { _, err := p.WriteAt([]byte("TAIL"), 4); second <- err }()
	if err := awaitManagedResult(t, second); err != nil {
		t.Fatal(err)
	}
	// Hash admission covers the entire piece, including both chunks.
	hash := make(chan error, 1)
	go func() { hash <- p.BeginVerification() }()
	defer func() {
		unblock.Do(func() { close(release) })
		if err := <-hash; err != nil {
			t.Error(err)
		}
	}()
	awaitManagedRanges(t, fs, 2)
	if len(hash) != 0 {
		t.Fatal("hash snapshot escaped pending chunk write")
	}
	unblock.Do(func() { close(release) })
	awaitManagedRanges(t, fs, 0)
	verifyManagedPiece(t, ts, info, 0, "GOODTAIL")
}

func TestManagedPayloadHashExcludesOnlyItsPiece(t *testing.T) {
	t.Run("hash", func(t *testing.T) { testManagedPayloadReadExclusion(t, false) })
	t.Run("verified-read", func(t *testing.T) { testManagedPayloadReadExclusion(t, true) })
}

func testManagedPayloadReadExclusion(t *testing.T, direct bool) {
	ts, fs, info, f := managedRangeFixture(t, false)
	p := ts.Piece(info.Piece(0)).(*filePieceImpl)
	if direct {
		verifyManagedPiece(t, ts, info, 0, "good")
	}
	if !direct {
		if err := p.BeginVerification(); err != nil {
			t.Fatal(err)
		}
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once, unblock sync.Once
	defer unblock.Do(func() { close(release) })
	f.read = func(b []byte, off int64) (int, error) {
		first := false
		once.Do(func() { first = true; close(entered) })
		if first {
			<-release
		}
		return f.managedPayloadFile.ReadAt(b, off)
	}
	hash := make(chan error, 1)
	go func() {
		var b bytes.Buffer
		var err error
		if direct {
			buf := make([]byte, 4)
			_, err = p.ReadAt(buf, 0)
			b.Write(buf)
		} else {
			_, err = p.WriteTo(&b)
		}
		if err == nil && b.String() != "good" {
			err = errors.New("hash saw concurrent mutation")
		}
		hash <- err
	}()
	defer func() {
		unblock.Do(func() { close(release) })
		if err := <-hash; err != nil {
			t.Error(err)
		}
	}()
	awaitWriteBarrier(t, entered)
	write := startManagedWrite(ts, info, 0, "evil")
	defer func() {
		unblock.Do(func() { close(release) })
		if err := <-write; err != nil {
			t.Error(err)
		}
	}()
	awaitManagedRanges(t, fs, 2)
	if err := awaitManagedResult(t, startManagedWrite(ts, info, 1, "TAIL")); err != nil {
		t.Fatal(err)
	}
	verifyManagedPiece(t, ts, info, 1, "TAIL")
	if s := ts.IOTimings(); s.WritePayload.Active != 0 || s.WriteLeaseWaitActive != 1 {
		t.Fatal(s)
	}
	unblock.Do(func() { close(release) })
	awaitManagedRanges(t, fs, 0)
	if err := p.MarkComplete(); err == nil || ts.CachedComplete(0) {
		t.Fatal("mutation after hashing retained stale completion")
	}
	if _, err := p.ReadAt(make([]byte, 4), 0); !errors.Is(err, ErrUnverifiedPayload) {
		t.Fatal("stale bytes readable", err)
	}
}

func TestManagedPayloadParallelWritesCheckpointBarrier(t *testing.T) {
	ts, fs, info, f := managedRangeFixture(t, false)
	verifyManagedPiece(t, ts, info, 0, "good")
	verifyManagedPiece(t, ts, info, 1, "tail")
	var revoked atomic.Uint32
	fs.resume.hooks = &FileResumeHooks{Revoke: func(mask []byte) (bool, error) {
		revoked.Or(uint32(mask[0]))
		return false, nil
	}}
	fs.resume.pending = []byte{3}
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	f.write = func(b []byte, off int64) (int, error) {
		if off == 0 {
			close(entered)
			<-release
		}
		if revoked.Load()&(1<<(off/4)) == 0 {
			return 0, errors.New("mutation preceded durable revocation")
		}
		return f.managedPayloadFile.WriteAt(b, off)
	}
	write := startManagedWrite(ts, info, 0, "GOOD")
	defer func() {
		unblock.Do(func() { close(release) })
		if err := <-write; err != nil {
			t.Error(err)
		}
	}()
	awaitWriteBarrier(t, entered)
	if fs.lifecycle.TryLock() {
		fs.lifecycle.Unlock()
		t.Fatal("write lost checkpoint mutation fence")
	}
	ctx, cancel := context.WithCancel(context.Background())
	checkpoint := make(chan error, 1)
	go func() {
		checkpoint <- ts.WithCheckpoint(ctx, []byte{3}, func(FileCheckpoint) error { t.Error("checkpoint published while write active"); return nil })
	}()
	cancel()
	if err := awaitManagedResult(t, checkpoint); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	unblock.Do(func() { close(release) })
	awaitManagedRanges(t, fs, 0)
	// The successful checkpoint excludes the changed/unverified piece, keeps
	// the neighbor and prevents a new writer throughout the publish callback.
	publishing, publishRelease := make(chan struct{}), make(chan struct{})
	var publishUnblock sync.Once
	defer publishUnblock.Do(func() { close(publishRelease) })
	sealed := make(chan error, 1)
	go func() {
		sealed <- ts.WithCheckpoint(context.Background(), []byte{3}, func(c FileCheckpoint) error {
			if c.Complete[0] != 2 {
				t.Error("checkpoint admitted changed piece", c.Complete)
			}
			close(publishing)
			<-publishRelease
			return nil
		})
	}()
	defer func() {
		publishUnblock.Do(func() { close(publishRelease) })
		if err := <-sealed; err != nil {
			t.Error(err)
		}
	}()
	awaitWriteBarrier(t, publishing)
	if fs.lifecycle.TryRLock() {
		fs.lifecycle.RUnlock()
		t.Fatal("checkpoint allowed new mutation admission")
	}
	second := startManagedWrite(ts, info, 1, "TAIL")
	defer func() {
		publishUnblock.Do(func() { close(publishRelease) })
		if err := <-second; err != nil {
			t.Error(err)
		}
		if revoked.Load() != 3 || ts.CachedComplete(0) || ts.CachedComplete(1) {
			t.Error("post-checkpoint write retained persisted/live trust")
		}
	}()
	if len(second) != 0 || revoked.Load() != 1 {
		t.Fatal("writer escaped checkpoint publication barrier")
	}
	publishUnblock.Do(func() { close(publishRelease) })
}

func TestManagedPayloadRangeFairness(t *testing.T) {
	var ranges managedPayloadRanges
	reader := ranges.lock(0, 4, false, nil)
	writerWaiting := make(chan struct{})
	writer := make(chan *managedPayloadRange, 1)
	go func() { writer <- ranges.lock(0, 4, true, func() { close(writerWaiting) }) }()
	awaitWriteBarrier(t, writerWaiting)
	readerWaiting := make(chan struct{})
	later := make(chan *managedPayloadRange, 1)
	go func() { later <- ranges.lock(0, 4, false, func() { close(readerWaiting) }) }()
	awaitWriteBarrier(t, readerWaiting)
	// An unrelated range must bypass both queued conflicting operations.
	neighbor := ranges.lock(4, 8, true, nil)
	ranges.unlock(neighbor)
	ranges.unlock(reader)
	var granted *managedPayloadRange
	select {
	case granted = <-writer:
	case <-time.After(5 * time.Second):
		t.Fatal("writer starved behind later reader")
	}
	if len(later) != 0 {
		t.Error("later reader overtook conflicting writer")
	}
	ranges.unlock(granted)
	select {
	case granted = <-later:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not resume")
	}
	ranges.unlock(granted)
	if ranges.head != nil || ranges.tail != nil {
		t.Fatal("range metadata retained after I/O drained")
	}
}

func TestManagedPayloadParallelWritesDuringSync(t *testing.T) {
	for _, mode := range []string{"neighbor-write", "same-piece-write", "sync-error"} {
		t.Run(mode, func(t *testing.T) {
			ts, _, info, f := managedRangeFixture(t, false)
			p := ts.Piece(info.Piece(0)).(*filePieceImpl)
			p.t.client.opts.PieceCompletion = persistentMapCompletion{NewMapPieceCompletion()}
			if err := p.BeginVerification(); err != nil {
				t.Fatal(err)
			}
			var b bytes.Buffer
			if _, err := p.WriteTo(&b); err != nil || b.String() != "good" {
				t.Fatal(b.String(), err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var unblock sync.Once
			defer unblock.Do(func() { close(release) })
			injected := errors.New("managed sync failed")
			f.syncIO = func() error {
				close(entered)
				<-release
				if mode == "sync-error" {
					return injected
				}
				return f.managedPayloadFile.Sync()
			}
			publish := make(chan error, 1)
			go func() { publish <- p.MarkComplete() }()
			defer func() {
				unblock.Do(func() { close(release) })
				err := <-publish
				if mode == "neighbor-write" {
					if err != nil || !ts.CachedComplete(0) {
						t.Error("independent write rejected valid hash", err)
					}
				} else if err == nil || ts.CachedComplete(0) {
					t.Error("failed barrier published completion", err)
				}
				if mode == "sync-error" && !errors.Is(err, injected) {
					t.Error(err)
				}
			}()
			awaitWriteBarrier(t, entered)
			index := 1
			if mode == "same-piece-write" {
				index = 0
			}
			if err := awaitManagedResult(t, startManagedWrite(ts, info, index, "next")); err != nil {
				t.Fatal(err)
			}
			if ts.CachedComplete(0) || ts.IOTimings().CompletionSyncActive != 1 {
				t.Fatal("completion published before sync")
			}
		})
	}
}

func TestManagedPayloadParallelWritesJoinResetAndClose(t *testing.T) {
	for _, reset := range []bool{false, true} {
		name := "close"
		if reset {
			name = "reset"
		}
		t.Run(name, func(t *testing.T) {
			ts, fs, info, f := managedRangeFixture(t, false)
			entered := make(chan int64, 2)
			release := []chan struct{}{make(chan struct{}), make(chan struct{})}
			var unblock [2]sync.Once
			defer func() {
				for i := range release {
					unblock[i].Do(func() { close(release[i]) })
				}
			}()
			f.write = func(b []byte, off int64) (int, error) {
				entered <- off
				<-release[off/4]
				return f.managedPayloadFile.WriteAt(b, off)
			}
			writes := []<-chan error{startManagedWrite(ts, info, 0, "GOOD"), startManagedWrite(ts, info, 1, "TAIL")}
			defer func() {
				for i := range release {
					unblock[i].Do(func() { close(release[i]) })
				}
				for _, done := range writes {
					<-done
				}
			}()
			for range 2 {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("same-file writers were serialized")
				}
			}
			done := make(chan error, 1)
			go func() {
				if reset {
					done <- ts.ResetManagedPayload(context.Background())
				} else {
					done <- ts.Close()
				}
			}()
			defer func() {
				for i := range release {
					unblock[i].Do(func() { close(release[i]) })
				}
				if err := <-done; err != nil {
					t.Error(err)
				}
				if _, err := f.ReadAt(make([]byte, 1), 0); !errors.Is(err, os.ErrClosed) {
					t.Error("descriptor survived lifecycle barrier", err)
				}
				awaitManagedRanges(t, fs, 0)
			}()
			unblock[0].Do(func() { close(release[0]) })
			// The second writer still borrows the shared descriptor.
			if f.closes.Load() != 0 {
				t.Fatal("closed an active parallel writer")
			}
			if len(done) != 0 {
				t.Fatal("lifecycle barrier did not join both writers")
			}
			unblock[1].Do(func() { close(release[1]) })
			// Join completion without consuming the channel used by deferred cleanup.
			deadline := time.Now().Add(5 * time.Second)
			for f.closes.Load() == 0 {
				if time.Now().After(deadline) {
					t.Fatal("lifecycle barrier never closed descriptors")
				}
				runtime.Gosched()
			}
		})
	}
}

func TestManagedPayloadParallelWriteErrorRevokesFile(t *testing.T) {
	for _, short := range []bool{false, true} {
		name := "write-error"
		if short {
			name = "short-write"
		}
		t.Run(name, func(t *testing.T) {
			ts, fs, info, f := managedRangeFixture(t, false)
			entered, release := make(chan struct{}), make(chan struct{})
			var unblock sync.Once
			defer unblock.Do(func() { close(release) })
			injected := errors.New("managed write failed")
			f.write = func(b []byte, off int64) (int, error) {
				if off != 0 {
					return f.managedPayloadFile.WriteAt(b, off)
				}
				close(entered)
				<-release
				if short {
					return f.managedPayloadFile.WriteAt(b[:1], off)
				}
				return 0, injected
			}
			done := startManagedWrite(ts, info, 0, "GOOD")
			defer func() {
				unblock.Do(func() { close(release) })
				want := injected
				if short {
					want = io.ErrShortWrite
				}
				if err := <-done; !errors.Is(err, want) {
					t.Error(err)
				}
			}()
			awaitWriteBarrier(t, entered)
			if err := awaitManagedResult(t, startManagedWrite(ts, info, 1, "TAIL")); err != nil {
				t.Fatal(err)
			}
			verifyManagedPiece(t, ts, info, 1, "TAIL")
			unblock.Do(func() { close(release) })
			awaitManagedRanges(t, fs, 0)
			if ts.CachedComplete(1) {
				t.Fatal("file I/O failure retained neighbor trust")
			}
			if f.closes.Load() != 1 || ts.IOTimings().FileCacheOpen != 0 {
				t.Fatal("failed descriptor not retired exactly once")
			}
			verifyManagedPiece(t, ts, info, 1, "TAIL")
		})
	}
}

// Keep the test descriptor contract honest on every supported platform.
var _ managedPayloadFile = (*os.File)(nil)

func TestManagedPayloadFailedOpenKeepsCacheAccounting(t *testing.T) {
	ts, fs, info := managedFixture(t, nil, 1, nil)
	if _, err := fs.acquireManagedFile(0, false); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if s := ts.IOTimings(); s.FileCacheOpen != 0 {
		t.Fatal("failed open counted a retained descriptor", s)
	}
	// An ordinary missing-file read must not latch a descriptor-close error.
	if _, err := ts.Piece(info.Piece(0)).WriteAt([]byte("good"), 0); err != nil {
		t.Fatal(err)
	}
	verifyManagedPiece(t, ts, info, 0, "good")
}

func TestManagedPayloadErrorRetiresAfterLastParallelBorrower(t *testing.T) {
	ts, fs, info, f := managedRangeFixture(t, false)
	entered := make(chan int64, 2)
	release := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var unblock [2]sync.Once
	injected := errors.New("one parallel writer failed")
	f.write = func(b []byte, off int64) (int, error) {
		entered <- off
		<-release[off/4]
		if off == 0 {
			return 0, injected
		}
		return f.managedPayloadFile.WriteAt(b, off)
	}
	failed := startManagedWrite(ts, info, 0, "GOOD")
	survivor := startManagedWrite(ts, info, 1, "TAIL")
	joined := false
	defer func() {
		for i := range release {
			unblock[i].Do(func() { close(release[i]) })
		}
		if !joined {
			<-failed
		}
		if err := <-survivor; err != nil {
			t.Error(err)
		}
	}()
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("parallel borrowed writes did not enter")
		}
	}
	unblock[0].Do(func() { close(release[0]) })
	err := awaitManagedResult(t, failed)
	joined = true
	if !errors.Is(err, injected) {
		t.Fatal(err)
	}
	if f.closes.Load() != 0 || ts.IOTimings().FileCacheOpen != 1 {
		t.Fatal("failed writer closed another writer's borrowed descriptor")
	}
	if _, err := f.Stat(); err != nil {
		t.Fatal("active borrower lost its handle", err)
	}
	unblock[1].Do(func() { close(release[1]) })
	awaitManagedRanges(t, fs, 0)
	if f.closes.Load() != 1 || ts.IOTimings().FileCacheOpen != 0 {
		t.Fatal("last borrower did not retire failed handle exactly once")
	}
	verifyManagedPiece(t, ts, info, 1, "TAIL")
}
