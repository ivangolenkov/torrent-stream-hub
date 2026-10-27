package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/metainfo"
)

// Stop one rooted writer at a selected stage; subsequent writers use the real
// backend. Stat is forwarded so instrumentation never removes signature fencing.
type blockedWriteIO struct {
	fileIo
	stage            string
	failStage        string
	fail             error
	legacyClose      bool
	stopAfter        int64
	calls            atomic.Int64
	once             sync.Once
	entered, release chan struct{}
}

func (b *blockedWriteIO) stop(stage string) {
	if b.stage == stage {
		if b.calls.Add(1) <= b.stopAfter {
			return
		}
		selected := false
		b.once.Do(func() { selected = true; close(b.entered) })
		if selected {
			<-b.release
		}
	}
}

func (b *blockedWriteIO) openForWrite(name string, size int64) (fileWriter, error) {
	b.stop("open")
	if b.failStage == "open" {
		return nil, b.fail
	}
	f, err := b.fileIo.openForWrite(name, size)
	if err != nil {
		return nil, err
	}
	h := blockedWriteHandle{fileWriter: f, barrier: b}
	if b.legacyClose {
		return leasedWriteHandle{h}, nil
	}
	return h, nil
}

// Deliberately does not forward the descriptor-only Close capability.
type leasedWriteHandle struct{ fileWriter }

func (f leasedWriteHandle) Stat() (os.FileInfo, error) {
	return f.fileWriter.(interface{ Stat() (os.FileInfo, error) }).Stat()
}

type blockedWriteHandle struct {
	fileWriter
	barrier *blockedWriteIO
}

func (f blockedWriteHandle) Stat() (os.FileInfo, error) {
	f.barrier.stop("stat")
	if f.barrier.failStage == "stat" {
		return nil, f.barrier.fail
	}
	return f.fileWriter.(interface{ Stat() (os.FileInfo, error) }).Stat()
}

func (f blockedWriteHandle) WriteAt(p []byte, off int64) (int, error) {
	f.barrier.stop("payload")
	if f.barrier.failStage == "payload" {
		return 0, f.barrier.fail
	}
	if f.barrier.failStage == "short-write" {
		return f.fileWriter.WriteAt(p[:1], off)
	}
	return f.fileWriter.WriteAt(p, off)
}

func (f blockedWriteHandle) Close() error {
	f.barrier.stop("close")
	err := f.fileWriter.Close()
	if f.barrier.failStage == "close" {
		return f.barrier.fail
	}
	return err
}

// Like a rooted os.File, this wrapper's Close only releases the descriptor.
func (f blockedWriteHandle) closeWithoutPayloadMutation() error { return f.Close() }

func writeConcurrencyFixture(t *testing.T) (TorrentImpl, *filePieceImpl, *filePieceImpl) {
	return writeConcurrencyInfoFixture(t, &metainfo.Info{Name: "data", Length: 8, PieceLength: 4, Pieces: make([]byte, 40)})
}

func writeConcurrencyInfoFixture(t *testing.T, info *metainfo.Info) (TorrentImpl, *filePieceImpl, *filePieceImpl) {
	t.Helper()
	return writeConcurrencyBackendFixture(t, info, true, false)
}

func writeConcurrencyBackendFixture(t *testing.T, info *metainfo.Info, verify, partFiles bool) (TorrentImpl, *filePieceImpl, *filePieceImpl) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	backend := NewFileOpts(NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: verify,
		UsePartFiles: g.Some(partFiles), NoCompletionRename: true, PieceCompletion: NewMapPieceCompletion()})
	t.Cleanup(func() { backend.Close() })
	ts, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ts.Close() })
	p := ts.Piece(info.Piece(0)).(*filePieceImpl)
	if _, err := (fileTorrentImplIO{p.t}).WriteAt([]byte("goodtail"), 0); err != nil {
		t.Fatal(err)
	}
	var q *filePieceImpl
	if info.NumPieces() > 1 {
		q = ts.Piece(info.Piece(1)).(*filePieceImpl)
	}
	return ts, p, q
}

func awaitWriteBarrier(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not reach barrier")
	}
}

func TestWriteCloseDoesNotLeasePayload(t *testing.T) {
	ts, p, q := writeConcurrencyFixture(t)
	b := &blockedWriteIO{fileIo: p.t.io, stage: "close", entered: make(chan struct{}), release: make(chan struct{})}
	p.t.io = b
	done := make(chan error, 1)
	go func() { _, err := p.WriteAt([]byte("GOOD"), 0); done <- err }()
	defer func() {
		close(b.release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	awaitWriteBarrier(t, b.entered)
	if !p.t.files[0].payloadMu.TryLock() {
		t.Fatal("descriptor Close holds file-wide payload lease and blocks neighboring writers")
	}
	p.t.files[0].payloadMu.Unlock()
	if p.t.lifecycle.TryLock() {
		p.t.lifecycle.Unlock()
		t.Fatal("Close lost lifecycle barrier")
	}
	if _, err := q.WriteAt([]byte("TAIL"), 0); err != nil {
		t.Fatal(err)
	}
	if err := p.BeginVerification(); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkComplete(); err != nil {
		t.Fatal(err)
	}
	if !ts.CachedComplete(0) {
		t.Fatal("unchanged piece could not publish during descriptor Close")
	}
	if _, err := p.WriteAt([]byte("evil"), 0); err != nil {
		t.Fatal(err)
	}
	if ts.CachedComplete(0) {
		t.Fatal("same-piece mutation retained trust")
	}
}

func TestWriteStageTimings(t *testing.T) {
	for _, stage := range []string{"open", "stat", "payload", "close"} {
		t.Run(stage, func(t *testing.T) {
			ts, p, _ := writeConcurrencyFixture(t)
			b := &blockedWriteIO{fileIo: p.t.io, stage: stage, entered: make(chan struct{}), release: make(chan struct{})}
			p.t.io = b
			before := ts.IOTimings()
			done := make(chan error, 1)
			go func() { _, err := p.WriteAt([]byte("GOOD"), 0); done <- err }()
			joined := false
			defer func() {
				if !joined {
					close(b.release)
					<-done
				}
			}()
			awaitWriteBarrier(t, b.entered)
			s := ts.IOTimings()
			active := map[string]IOStageTimings{"open": s.WriteOpen, "stat": s.WriteStat, "payload": s.WritePayload, "close": s.WriteClose}[stage]
			if active.Active != 1 {
				t.Fatalf("blocked %s not observable: %+v", stage, s)
			}
			wantHold := int64(1)
			if stage == "close" {
				wantHold = 0
			}
			if s.WriteLeaseHold.Active != wantHold {
				t.Fatalf("wrong lease scope: %+v", s)
			}
			if stage != "close" && p.t.files[0].payloadMu.TryLock() {
				p.t.files[0].payloadMu.Unlock()
				t.Fatal("mutation/signature evidence escaped exclusive lease")
			}
			if ts.IOTimings() != s {
				t.Fatal("polling changed stage counters")
			}
			close(b.release)
			err := <-done
			joined = true
			if err != nil {
				t.Fatal(err)
			}
			after := ts.IOTimings()
			old := []IOStageTimings{before.WriteLeaseHold, before.WritePrepare, before.WriteGuard, before.WriteOpen, before.WriteStat, before.WritePayload, before.WriteAccount, before.WriteClose}
			got := []IOStageTimings{after.WriteLeaseHold, after.WritePrepare, after.WriteGuard, after.WriteOpen, after.WriteStat, after.WritePayload, after.WriteAccount, after.WriteClose}
			for i, a := range got {
				wantCount := int64(1)
				if i == 4 {
					wantCount = 2
				}
				if a.Count-old[i].Count != wantCount || a.Active != 0 || a.Errors != 0 || a.NS < old[i].NS || a.MaxNS < old[i].MaxNS || a.MaxNS > a.NS {
					t.Fatalf("stage %d counters: before=%+v after=%+v", i, old[i], a)
				}
			}
		})
	}
}

func TestWriteStageErrors(t *testing.T) {
	for _, stage := range []string{"open", "stat", "payload", "short-write", "close"} {
		t.Run(stage, func(t *testing.T) {
			ts, p, _ := writeConcurrencyFixture(t)
			injected := errors.New("injected write stage failure")
			p.t.io = &blockedWriteIO{fileIo: p.t.io, failStage: stage, fail: injected}
			before := ts.IOTimings()
			n, err := p.WriteAt([]byte("GOOD"), 0)
			want := injected
			if stage == "stat" {
				want = nil
			} // Existing behavior: no before/after evidence, no trust.
			if stage == "short-write" {
				want = io.ErrShortWrite
			}
			if !errors.Is(err, want) {
				t.Fatalf("write(%s): n=%d err=%v want=%v", stage, n, err, want)
			}
			after := ts.IOTimings()
			if after.WriteLeaseHold.Active != 0 || after.WriteClose.Active != 0 || after.WritePayload.Active != 0 || after.WriteOpen.Active != 0 || after.WriteStat.Active != 0 {
				t.Fatal("leaked active counter", after)
			}
			if stage != "open" && after.WriteClose.Count != before.WriteClose.Count+1 {
				t.Fatal("descriptor not closed", after)
			}
			if stage == "open" && after.WriteOpen.Errors != before.WriteOpen.Errors+1 || stage == "stat" && after.WriteStat.Errors != before.WriteStat.Errors+2 || (stage == "payload" || stage == "short-write") && after.WritePayload.Errors != before.WritePayload.Errors+1 || stage == "close" && after.WriteClose.Errors != before.WriteClose.Errors+1 {
				t.Fatal("stage error not counted", after)
			}
			if ts.CachedComplete(0) {
				t.Fatal("write installed completion")
			}
			if !p.t.files[0].payloadMu.TryLock() {
				t.Fatal("write error leaked payload lease")
			}
			p.t.files[0].payloadMu.Unlock()
		})
	}
}

func TestWriteDurableBarrierStages(t *testing.T) {
	for _, guard := range []bool{false, true} {
		name := "prepare-revoke"
		if guard {
			name = "file-guard-revoke"
		}
		t.Run(name, func(t *testing.T) {
			ts, p, _ := writeConcurrencyFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			injected := errors.New("durable revoke failed")
			p.t.resume.hooks = &FileResumeHooks{Revoke: func([]byte) (bool, error) {
				close(entered)
				<-release
				return false, injected
			}}
			p.t.resume.pending = []byte{1}
			// The neighboring persisted piece survives per-piece revoke; the
			// file guard must revoke it before any creation/extension risk.
			if guard {
				p.t.resume.pending[0] = 2
			}
			before := ts.IOTimings()
			done := make(chan error, 1)
			go func() {
				n, err := p.WriteAt([]byte("EVIL"), 0)
				if n != 0 {
					err = errors.New("payload changed before durable revoke")
				}
				done <- err
			}()
			joined := false
			defer func() {
				if !joined {
					close(release)
					<-done
				}
			}()
			awaitWriteBarrier(t, entered)
			s := ts.IOTimings()
			active := s.WritePrepare
			if guard {
				active = s.WriteGuard
			}
			if active.Active != 1 || s.WriteLeaseHold.Active != 1 {
				t.Fatal("durable barrier not observable", s)
			}
			close(release)
			err := <-done
			joined = true
			if !errors.Is(err, injected) {
				t.Fatal(err)
			}
			after := ts.IOTimings()
			failed := after.WritePrepare
			if guard {
				failed = after.WriteGuard
			}
			if failed.Errors != 1 || failed.Active != 0 || after.WriteLeaseHold.Active != 0 || after.WritePayload.Count != before.WritePayload.Count || after.WriteOpen.Count != before.WriteOpen.Count {
				t.Fatal("durable failure bypassed barrier", after)
			}
			data, err := os.ReadFile(p.t.files[0].safeOsPath)
			if err != nil || string(data) != "goodtail" {
				t.Fatalf("revoke failed after mutation: %q %v", data, err)
			}
		})
	}
}

func TestWriteSharedBoundaryCloseLease(t *testing.T) {
	for _, last := range []bool{false, true} {
		name := "intermediate-close"
		if last {
			name = "final-close"
		}
		t.Run(name, func(t *testing.T) {
			info := &metainfo.Info{Name: "bundle", PieceLength: 8, Pieces: make([]byte, 20), Files: []metainfo.FileInfo{{Length: 4, Path: []string{"a"}}, {Length: 4, Path: []string{"b"}}}}
			ts, p, _ := writeConcurrencyInfoFixture(t, info)
			b := &blockedWriteIO{fileIo: p.t.io, stage: "close", entered: make(chan struct{}), release: make(chan struct{})}
			if last {
				b.stopAfter = 1
			}
			p.t.io = b
			done := make(chan error, 1)
			go func() { _, err := p.WriteAt([]byte("GOODTAIL"), 0); done <- err }()
			defer func() {
				close(b.release)
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			awaitWriteBarrier(t, b.entered)
			for i := range p.t.files {
				got := p.t.files[i].payloadMu.TryLock()
				if got {
					p.t.files[i].payloadMu.Unlock()
				}
				if got != last {
					t.Fatalf("file %d: lease available=%v want=%v", i, got, last)
				}
			}
			if last {
				if err := p.BeginVerification(); err != nil {
					t.Fatal(err)
				}
				if err := p.MarkComplete(); err != nil {
					t.Fatal(err)
				}
				if !ts.CachedComplete(0) {
					t.Fatal("shared-boundary evidence lost after write accounting")
				}
			}
		})
	}
}

func TestWriteLegacyCloseRetainsPayloadLease(t *testing.T) {
	for _, mode := range []string{"unknown-writer", "part-files", "legacy-verification"} {
		t.Run(mode, func(t *testing.T) {
			info := &metainfo.Info{Name: "data", Length: 8, PieceLength: 4, Pieces: make([]byte, 40)}
			_, p, _ := writeConcurrencyBackendFixture(t, info, mode != "legacy-verification", mode == "part-files")
			b := &blockedWriteIO{fileIo: p.t.io, stage: "close", legacyClose: mode == "unknown-writer", entered: make(chan struct{}), release: make(chan struct{})}
			p.t.io = b
			done := make(chan error, 1)
			go func() { _, err := p.WriteAt([]byte("GOOD"), 0); done <- err }()
			defer func() {
				close(b.release)
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			awaitWriteBarrier(t, b.entered)
			if p.t.files[0].payloadMu.TryLock() {
				p.t.files[0].payloadMu.Unlock()
				t.Fatal("backend without descriptor-only capability lost close exclusion")
			}
		})
	}
}

// Test-only reconstruction of the old Close lease scope, with an artificial
// delay. This measures neighbor latency, not NAS or end-to-end throughput.
type delayedWriteCloseIO struct {
	fileIo
	legacy  bool
	entered chan struct{}
}

func (b delayedWriteCloseIO) openForWrite(name string, size int64) (fileWriter, error) {
	f, err := b.fileIo.openForWrite(name, size)
	if err != nil {
		return nil, err
	}
	h := &delayedWriteCloseHandle{fileWriter: f, entered: b.entered}
	if b.legacy {
		return leasedWriteHandle{h}, nil
	}
	return h, nil
}

type delayedWriteCloseHandle struct {
	fileWriter
	entered chan struct{}
	delay   bool
}

func (f *delayedWriteCloseHandle) Stat() (os.FileInfo, error) {
	return f.fileWriter.(interface{ Stat() (os.FileInfo, error) }).Stat()
}
func (f *delayedWriteCloseHandle) WriteAt(p []byte, off int64) (int, error) {
	f.delay = off == 0
	return f.fileWriter.WriteAt(p, off)
}
func (f *delayedWriteCloseHandle) Close() error {
	if f.delay {
		f.entered <- struct{}{}
		time.Sleep(20 * time.Millisecond)
	}
	return f.fileWriter.Close()
}
func (f *delayedWriteCloseHandle) closeWithoutPayloadMutation() error { return f.Close() }

func BenchmarkWriteCloseNeighbor(b *testing.B) {
	for _, legacy := range []bool{true, false} {
		name := "narrowed-scope"
		if legacy {
			name = "legacy-close-scope"
		}
		b.Run(name, func(b *testing.B) {
			dir := b.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				b.Fatal(err)
			}
			backend := NewFileOpts(NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, UsePartFiles: g.Some(false), NoCompletionRename: true, PieceCompletion: NewMapPieceCompletion()})
			defer backend.Close()
			info := &metainfo.Info{Name: "data", Length: 8, PieceLength: 4, Pieces: make([]byte, 40)}
			ts, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
			if err != nil {
				b.Fatal(err)
			}
			defer ts.Close()
			p := ts.Piece(info.Piece(0)).(*filePieceImpl)
			q := ts.Piece(info.Piece(1))
			entered := make(chan struct{}, 1)
			p.t.io = delayedWriteCloseIO{fileIo: p.t.io, legacy: legacy, entered: entered}
			var neighborNS int64
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				done := make(chan error, 1)
				go func() { _, err := p.WriteAt([]byte("good"), 0); done <- err }()
				<-entered
				start := time.Now()
				_, writeErr := q.WriteAt([]byte("tail"), 0)
				neighborNS += time.Since(start).Nanoseconds()
				closeErr := <-done
				if writeErr != nil || closeErr != nil {
					b.Fatal(writeErr, closeErr)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(neighborNS)/float64(b.N)/1e3, "neighbor-us/op")
		})
	}
}
