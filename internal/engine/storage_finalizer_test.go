package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/fastresume"
	"torrent-stream-hub/internal/payload"
)

type controlledStorageFinalizer struct {
	shutdown func(context.Context) error
	wait     func() error
}

func (f controlledStorageFinalizer) Shutdown(ctx context.Context) error { return f.shutdown(ctx) }
func (f controlledStorageFinalizer) Wait() error                        { return f.wait() }

func finalizerEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := New(&config.Config{DownloadDir: t.TempDir(), BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true, BTDisableTCP: true, BTDisableUTP: true, BTRetrackersMode: "off"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

func finalizerReceive[T any](t *testing.T, c <-chan T) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("storage finalizer barrier timed out")
		var zero T
		return zero
	}
}

func TestStorageFinalizerDeadlineStillJoinsBeforeResourceClose(t *testing.T) {
	e := finalizerEngine(t)
	tasksRelease, joinedRelease := make(chan struct{}), make(chan struct{})
	var tasksOnce, joinedOnce sync.Once
	unblockTasks := func() { tasksOnce.Do(func() { close(tasksRelease) }) }
	unblockJoin := func() { joinedOnce.Do(func() { close(joinedRelease) }) }
	t.Cleanup(func() { unblockTasks(); unblockJoin() })
	if !e.StartTask(func(ctx context.Context) { <-ctx.Done(); <-tasksRelease }) {
		t.Fatal("task rejected")
	}
	shutdown, joining := make(chan context.Context, 1), make(chan struct{})
	finalizer := controlledStorageFinalizer{
		shutdown: func(ctx context.Context) error {
			if !e.admissionMu.TryLock() {
				t.Error("final drain retained admissionMu")
			} else {
				e.admissionMu.Unlock()
			}
			if !e.mu.TryLock() {
				t.Error("final drain retained engine mutex")
			} else {
				e.mu.Unlock()
			}
			if e.RegisterStorageFinalizer(nil) {
				t.Error("final drain reopened registration")
			}
			shutdown <- ctx
			return context.DeadlineExceeded
		},
		wait: func() error { close(joining); <-joinedRelease; return errors.New("failed flush joined") },
	}
	if e.RegisterStorageFinalizer(nil) || !e.RegisterStorageFinalizer(finalizer) || e.RegisterStorageFinalizer(finalizer) {
		t.Fatal("invalid finalizer admission")
	}
	closed := make(chan struct{})
	go func() { e.Close(); close(closed) }()
	finalizerReceive(t, e.ctx.Done())
	ctx := finalizerReceive(t, shutdown)
	if ctx.Err() != nil {
		t.Fatal("finalizer used cancelled runtime context", ctx.Err())
	}
	// Stop/drain must be signalled before task join, but shared resources are
	// joined only after ordinary tasks have returned.
	select {
	case <-joining:
		t.Fatal("finalizer resource join overtook engine task join")
	default:
	}
	unblockTasks()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("missing shutdown budget")
	}
	finalizerReceive(t, joining)
	if e.RegisterStorageFinalizer(finalizer) {
		t.Fatal("shutdown admitted finalizer")
	}
	lock, err := payload.LockInstance(e.cfg.DownloadDir)
	if lock != nil {
		_ = lock.Close()
	}
	if !errors.Is(err, payload.ErrInstanceLocked) {
		t.Fatal("drain error released instance ownership before join", err)
	}
	select {
	case <-e.client.Closed():
		t.Fatal("client closed before finalizer joined")
	default:
	}
	unblockJoin()
	finalizerReceive(t, closed)
	if ctx.Err() == nil {
		t.Fatal("finalizer context leaked after close")
	}
	lock, err = payload.LockInstance(e.cfg.DownloadDir)
	if err != nil {
		t.Fatal("joined finalizer leaked instance lock", err)
	}
	_ = lock.Close()
}

func TestStorageFinalizerOwnsSealWorkerLifetime(t *testing.T) {
	e := finalizerEngine(t)
	entered, release := make(chan context.Context, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	w, err := fastresume.NewSealWorker(func(ctx context.Context, owner string) error {
		if e.ctx.Err() == nil || owner != "owner" {
			t.Error("worker drain did not follow producer shutdown")
		}
		entered <- ctx
		<-release
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unblock(); _ = w.Shutdown(context.Background()); _ = w.Wait() })
	if !e.RegisterStorageFinalizer(w) || !w.MarkDirty("owner") {
		t.Fatal("worker admission failed")
	}
	closed := make(chan struct{})
	go func() { e.Close(); close(closed) }()
	ctx := finalizerReceive(t, entered)
	if ctx.Err() != nil {
		t.Fatal("orderly drain was cancelled before capture", ctx.Err())
	}
	select {
	case <-w.Done():
		t.Fatal("worker joined before callback returned")
	default:
	}
	unblock()
	finalizerReceive(t, closed)
	if err := w.Wait(); err != nil {
		t.Fatal(err)
	}
	if w.MarkDirty("late") {
		t.Fatal("closed engine left checkpoint admission open")
	}
}

func TestStorageFinalizerRejectedRegistrationRetainsCallerOwnership(t *testing.T) {
	e := finalizerEngine(t)
	e.BeginShutdown()
	w, err := fastresume.NewSealWorker(func(context.Context, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Shutdown(context.Background()); _ = w.Wait() }()
	if e.RegisterStorageFinalizer(w) {
		t.Fatal("registration crossed shutdown fence")
	}
	e.Close()
	if !w.MarkDirty("caller-owned") {
		t.Fatal("engine stopped a worker whose ownership was not transferred")
	}
}

func TestStorageFinalizerRegistrationShutdownRace(t *testing.T) {
	for i := 0; i < 20; i++ {
		e := finalizerEngine(t)
		var shutdowns, joins atomic.Int32
		finalizer := controlledStorageFinalizer{
			shutdown: func(context.Context) error { shutdowns.Add(1); return nil },
			wait:     func() error { joins.Add(1); return nil },
		}
		start, closed := make(chan struct{}), make(chan struct{})
		accepted := make(chan bool, 1)
		go func() { <-start; accepted <- e.RegisterStorageFinalizer(finalizer) }()
		go func() { <-start; e.Close(); close(closed) }()
		close(start)
		owned := finalizerReceive(t, accepted)
		finalizerReceive(t, closed)
		e.Close()
		want := int32(0)
		if owned {
			want = 1
		}
		if shutdowns.Load() != want || joins.Load() != want {
			t.Fatal("registration ownership was lost or finalized twice", owned, shutdowns.Load(), joins.Load())
		}
	}
}

func TestStorageFinalizerStopPrecedesCoordinatorStyleTaskJoin(t *testing.T) {
	e := finalizerEngine(t)
	stopRequested, taskJoined := make(chan struct{}), make(chan struct{})
	var once sync.Once
	requestStop := func() { once.Do(func() { close(stopRequested) }) }
	t.Cleanup(requestStop)
	if !e.StartTask(func(ctx context.Context) {
		<-ctx.Done()
		// Model coordinator cleanup retaining a reference until a seal's
		// shutdown request releases it. Joining first would deadlock.
		<-stopRequested
		close(taskJoined)
	}) {
		t.Fatal("task rejected")
	}
	if !e.RegisterStorageFinalizer(controlledStorageFinalizer{
		shutdown: func(context.Context) error { requestStop(); return nil },
		wait:     func() error { <-taskJoined; return nil },
	}) {
		t.Fatal("finalizer rejected")
	}
	closed := make(chan struct{})
	go func() { e.Close(); close(closed) }()
	finalizerReceive(t, closed)
}
