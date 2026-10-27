package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/metainfo"
)

type persistentMapCompletion struct{ PieceCompletion }

func (persistentMapCompletion) Persistent() bool { return true }

type barrierFlushIO struct {
	fileIo
	entered   chan struct{}
	release   chan struct{}
	err       error
	once      *sync.Once
	afterSync bool
}

func (b barrierFlushIO) flush(name string, off, length int64) error {
	if b.afterSync {
		if err := b.fileIo.flush(name, off, length); err != nil {
			return err
		}
	}
	b.once.Do(func() { close(b.entered) })
	<-b.release
	if b.err != nil {
		return b.err
	}
	if b.afterSync {
		return nil
	}
	return b.fileIo.flush(name, off, length)
}

func TestCompletionSyncDoesNotLeasePayload(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	backend := NewFileOpts(NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true,
		UsePartFiles: g.Some(false), NoCompletionRename: true,
		PieceCompletion: persistentMapCompletion{NewMapPieceCompletion()}})
	defer backend.Close()
	info := &metainfo.Info{Name: "data", Length: 8, PieceLength: 4, Pieces: make([]byte, 40)}
	ts, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	p0 := ts.Piece(info.Piece(0)).(*filePieceImpl)
	p1 := ts.Piece(info.Piece(1)).(*filePieceImpl)
	if _, err := p0.WriteAt([]byte("good"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := p1.WriteAt([]byte("tail"), 0); err != nil {
		t.Fatal(err)
	}
	if err := p0.BeginVerification(); err != nil {
		t.Fatal(err)
	}
	barrier := barrierFlushIO{fileIo: p0.t.io, entered: make(chan struct{}), release: make(chan struct{}), once: new(sync.Once)}
	p0.t.io = barrier
	done := make(chan error, 1)
	go func() { done <- p0.MarkComplete() }()
	<-barrier.entered
	defer func() {
		close(barrier.release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	// Deterministic proof, not a throughput/timing threshold: while Sync is
	// stopped, no payload lease may prevent an independent writer acquiring it.
	if !p0.t.files[0].payloadMu.TryLock() {
		t.Fatal("completion fsync holds the file payload lease and blocks all neighboring writes")
	}
	p0.t.files[0].payloadMu.Unlock()
	if p0.t.lifecycle.TryLock() {
		p0.t.lifecycle.Unlock()
		t.Fatal("sync lost Close/seal lifecycle fence")
	}
	if s := ts.IOTimings(); s.CompletionSyncActive != 1 || s.CompletionSyncCount != 0 {
		t.Fatalf("blocked sync not observable: %+v", s)
	}
	if _, err := p1.WriteAt([]byte("TAIL"), 0); err != nil {
		t.Fatal(err)
	}
}

func TestCompletionSyncRevalidatesAfterUnlockedIO(t *testing.T) {
	for _, afterSync := range []bool{false, true} {
		for _, mode := range []string{"same-piece-write", "external-replace", "external-truncate", "revoke", "sync-error", "neighbor-write", "neighbor-boundary-write"} {
			phase := "during-sync/"
			if afterSync {
				phase = "after-sync/"
			}
			t.Run(phase+mode, func(t *testing.T) {
				dir := t.TempDir()
				root, err := os.OpenRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				backend := NewFileOpts(NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true,
					UsePartFiles: g.Some(false), NoCompletionRename: true,
					PieceCompletion: persistentMapCompletion{NewMapPieceCompletion()}})
				defer backend.Close()
				info := &metainfo.Info{Name: "data", Length: 8, PieceLength: 4, Pieces: make([]byte, 40)}
				if mode == "neighbor-boundary-write" {
					info.Length = 0
					info.Files = []metainfo.FileInfo{{Length: 2, Path: []string{"a"}}, {Length: 6, Path: []string{"b"}}}
				}
				ts, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
				if err != nil {
					t.Fatal(err)
				}
				defer ts.Close()
				p := ts.Piece(info.Piece(0)).(*filePieceImpl)
				if _, err := p.WriteAt([]byte("goodtail"), 0); err != nil {
					t.Fatal(err)
				}
				if err := p.BeginVerification(); err != nil {
					t.Fatal(err)
				}
				barrier := barrierFlushIO{fileIo: p.t.io, entered: make(chan struct{}), release: make(chan struct{}), once: new(sync.Once), afterSync: afterSync}
				if mode == "sync-error" {
					barrier.err = io.ErrClosedPipe
				}
				p.t.io = barrier
				done := make(chan error, 1)
				go func() { done <- p.MarkComplete() }()
				<-barrier.entered
				// Always release/join before closing the backend, also after Fatal.
				joined := false
				defer func() {
					if !joined {
						close(barrier.release)
						<-done
					}
				}()
				switch mode {
				case "same-piece-write":
					_, err = p.WriteAt([]byte("evil"), 0)
				case "neighbor-write", "neighbor-boundary-write":
					_, err = ts.Piece(info.Piece(1)).WriteAt([]byte("TAIL"), 0)
				case "external-replace":
					other := filepath.Join(dir, "other")
					if err = os.WriteFile(other, []byte("eviltail"), 0600); err == nil {
						err = os.Rename(other, filepath.Join(dir, "data"))
					}
				case "external-truncate":
					err = os.Truncate(filepath.Join(dir, "data"), 2)
				case "revoke":
					p.t.freshMu.Lock()
					delete(p.t.hashSnapshot, 0)
					p.t.freshMu.Unlock()
				}
				if err != nil {
					t.Fatal(err)
				}
				close(barrier.release)
				err = <-done
				joined = true
				wantComplete := mode == "neighbor-write" || mode == "neighbor-boundary-write"
				if (err == nil) != wantComplete || ts.CachedComplete(0) != wantComplete {
					t.Fatalf("stale result/trust after %s: err=%v complete=%v", mode, err, ts.CachedComplete(0))
				}
				s := ts.IOTimings()
				if s.CompletionSyncCount != 1 || s.CompletionSyncActive != 0 || s.CompletionSyncNS <= 0 || s.CompletionSyncMaxNS != s.CompletionSyncNS {
					t.Fatalf("sync counters incorrect: %+v", s)
				}
				if mode == "sync-error" {
					if !errors.Is(err, io.ErrClosedPipe) || s.CompletionSyncErrors != 1 {
						t.Fatalf("sync failure hidden: %v %+v", err, s)
					}
				} else if s.CompletionSyncErrors != 0 {
					t.Fatal(s)
				}
				if ts.IOTimings() != s {
					t.Fatal("polling changed counters")
				}
			})
		}
	}
}

func TestWriteLeaseWaitTimings(t *testing.T) {
	_, _, info, ts := verifiedFixture(t)
	p := ts.Piece(info.Piece(0)).(*filePieceImpl)
	p.t.files[0].payloadMu.RLock()
	done := make(chan error, 1)
	go func() { _, err := p.WriteAt([]byte("good"), 0); done <- err }()
	deadline := time.Now().Add(3 * time.Second)
	for ts.IOTimings().WriteLeaseWaitActive != 1 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	s := ts.IOTimings()
	p.t.files[0].payloadMu.RUnlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s.WriteLeaseWaitActive != 1 {
		t.Fatal("waiting writer not observable", s)
	}
	s = ts.IOTimings()
	if s.WriteLeaseWaitCount != 1 || s.WriteLeaseWaitActive != 0 || s.WriteLeaseWaitNS <= 0 || s.WriteLeaseWaitMaxNS != s.WriteLeaseWaitNS {
		t.Fatal(s)
	}
	if _, err := p.WriteAt([]byte("good"), 0); err != nil {
		t.Fatal(err)
	}
	if ts.IOTimings().WriteLeaseWaitCount != 1 {
		t.Fatal("uncontended write counted as waiting")
	}
}

// Benchmark-only reconstruction of the OLD lease scope during a controlled
// 20ms sync delay. Runtime always uses the narrowed scope; this does not claim
// to reproduce NAS fsync latency or measure public-swarm throughput.
type delayedCompletionIO struct {
	fileIo
	torrent *fileTorrentImpl
	legacy  bool
	entered chan struct{}
}

func (s delayedCompletionIO) flush(name string, off, n int64) error {
	if s.legacy {
		unlock := s.torrent.lockPayloadRange(0, 4, false)
		defer unlock()
	}
	s.entered <- struct{}{}
	time.Sleep(20 * time.Millisecond)
	return s.fileIo.flush(name, off, n)
}

func BenchmarkCompletionSyncNeighborWrite(b *testing.B) {
	for _, legacy := range []bool{true, false} {
		name := "narrowed-scope"
		if legacy {
			name = "legacy-sync-scope"
		}
		b.Run(name, func(b *testing.B) {
			dir := b.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				b.Fatal(err)
			}
			backend := NewFileOpts(NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true,
				UsePartFiles: g.Some(false), NoCompletionRename: true,
				PieceCompletion: persistentMapCompletion{NewMapPieceCompletion()}})
			defer backend.Close()
			info := &metainfo.Info{Name: "data", Length: 8, PieceLength: 4, Pieces: make([]byte, 40)}
			ts, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
			if err != nil {
				b.Fatal(err)
			}
			defer ts.Close()
			p := ts.Piece(info.Piece(0)).(*filePieceImpl)
			neighbor := ts.Piece(info.Piece(1))
			if _, err := p.WriteAt([]byte("goodtail"), 0); err != nil {
				b.Fatal(err)
			}
			entered := make(chan struct{}, 1)
			p.t.io = delayedCompletionIO{p.t.io, p.t, legacy, entered}
			var writerNS int64
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := p.BeginVerification(); err != nil {
					b.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- p.MarkComplete() }()
				<-entered
				start := time.Now()
				_, writeErr := neighbor.WriteAt([]byte("TAIL"), 0)
				writerNS += time.Since(start).Nanoseconds()
				commitErr := <-done
				if writeErr != nil || commitErr != nil {
					b.Fatal(writeErr, commitErr)
				}
			}
			b.StopTimer()
			s := ts.IOTimings()
			b.ReportMetric(float64(writerNS)/float64(b.N)/1e3, "neighbor-us/op")
			b.ReportMetric(float64(s.WriteLeaseWaitNS)/float64(b.N)/1e3, "write-wait-us/op")
			b.ReportMetric(float64(s.CompletionSyncNS)/float64(b.N)/1e3, "sync-us/op")
		})
	}
}
