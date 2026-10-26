package fastresume

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func workerReceive[T any](t *testing.T, c <-chan T) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("worker barrier timed out")
		var zero T
		return zero
	}
}

func workerTick(t *testing.T, c chan<- time.Time) {
	t.Helper()
	select {
	case c <- time.Time{}:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not accept tick")
	}
}

func TestSealWorkerCoalescesAndRetainsConcurrentDirty(t *testing.T) {
	ticks := make(chan time.Time)
	entered, release := make(chan string, 4), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	w := newSealWorker(func(_ context.Context, owner string) error { entered <- owner; <-release; return nil }, ticks, func() {})
	t.Cleanup(func() { unblock(); _ = w.Shutdown(context.Background()) })
	for i := 0; i < 20; i++ {
		if !w.MarkDirty("a") {
			t.Fatal("coalesced request rejected")
		}
	}
	workerTick(t, ticks)
	if workerReceive(t, entered) != "a" {
		t.Fatal("wrong owner")
	}
	// This change is newer than the captured generation and needs another seal.
	if !w.MarkDirty("a") || !w.MarkDirty("b") {
		t.Fatal("dirty request lost")
	}
	unblock()
	workerTick(t, ticks) // accepted only after the first batch has joined
	if workerReceive(t, entered) != "b" || workerReceive(t, entered) != "a" {
		t.Fatal("FIFO/coalescing changed")
	}
	workerTick(t, ticks)
	if err := w.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(entered) != 0 {
		t.Fatal("unchanged owner resealed")
	}
}

func TestSealWorkerBoundIncludesActiveOwner(t *testing.T) {
	ticks := make(chan time.Time)
	entered, release := make(chan string, MaxPendingOwners), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	w := newSealWorker(func(_ context.Context, owner string) error { entered <- owner; <-release; return nil }, ticks, func() {})
	t.Cleanup(func() { unblock(); _ = w.Shutdown(context.Background()) })
	if w.MarkDirty("") || w.MarkDirty(strings.Repeat("x", MaxIdentityLength+1)) {
		t.Fatal("accepted invalid owner")
	}
	w.MarkDirty("active")
	workerTick(t, ticks)
	if workerReceive(t, entered) != "active" {
		t.Fatal("wrong active owner")
	}
	for i := 1; i < MaxPendingOwners; i++ {
		if !w.MarkDirty(fmt.Sprint(i)) {
			t.Fatal("premature saturation", i)
		}
	}
	if w.MarkDirty("overflow") || !w.MarkDirty("1") {
		t.Fatal("bound/coalescing broken")
	}
	unblock()
	if err := w.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(entered) != MaxPendingOwners-1 {
		t.Fatal("queue lost admitted owners", len(entered))
	}
	if w.MarkDirty("late") {
		t.Fatal("shutdown admitted work")
	}
}

func TestSealWorkerRetryAndDrainFailure(t *testing.T) {
	ticks := make(chan time.Time)
	entered := make(chan string, 4)
	injected := errors.New("flush failed")
	w := newSealWorker(func(_ context.Context, owner string) error { entered <- owner; return injected }, ticks, func() {})
	t.Cleanup(func() { _ = w.Shutdown(context.Background()) })
	w.MarkDirty("a")
	workerTick(t, ticks)
	workerReceive(t, entered)
	workerTick(t, ticks)
	workerReceive(t, entered)
	if err := w.Shutdown(context.Background()); !errors.Is(err, injected) {
		t.Fatal("drain failure hidden", err)
	}
	if workerReceive(t, entered) != "a" || len(entered) != 0 {
		t.Fatal("failed owner retried unboundedly during drain")
	}
}

func TestSealWorkerShutdownDeadlineRetainsCallback(t *testing.T) {
	ticks := make(chan time.Time)
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	w := newSealWorker(func(ctx context.Context, _ string) error { entered <- ctx; <-release; return ctx.Err() }, ticks, func() {})
	t.Cleanup(func() { unblock(); _ = w.Shutdown(context.Background()) })
	w.MarkDirty("blocked-syscall")
	workerTick(t, ticks)
	jobCtx := workerReceive(t, entered)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	go func() { returned <- w.Shutdown(ctx) }()
	workerReceive(t, w.stopping)
	if w.MarkDirty("late") {
		t.Fatal("admission not stopped")
	}
	cancel()
	if err := workerReceive(t, returned); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	workerReceive(t, jobCtx.Done())
	select {
	case <-w.Done():
		t.Fatal("deadline released unjoined callback")
	default: // callback is causally blocked by release, not a scheduling assertion
	}
	unblock()
	if err := w.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatal("joined task hid cancellation", err)
	}
	if err := w.Shutdown(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatal("second shutdown changed result", err)
	}
}

func TestSealWorkerShutdownIndependentOfRuntime(t *testing.T) {
	runtime, stopRuntime := context.WithCancel(context.Background())
	stopRuntime()
	if runtime.Err() == nil {
		t.Fatal("fixture runtime not stopped")
	}
	called := make(chan error, 1)
	w, err := NewSealWorker(func(ctx context.Context, _ string) error { called <- ctx.Err(); return ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	w.MarkDirty("a")
	if err := w.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := workerReceive(t, called); err != nil {
		t.Fatal("used stopped runtime context", err)
	}
	if _, err := NewSealWorker(nil); err == nil {
		t.Fatal("nil callback accepted")
	}
}

func TestSealWorkerConcurrentDirtyAndSaturationRetry(t *testing.T) {
	ticks := make(chan time.Time)
	entered, release := make(chan string, MaxPendingOwners+1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	w := newSealWorker(func(_ context.Context, owner string) error { entered <- owner; <-release; return nil }, ticks, func() {})
	t.Cleanup(func() { unblock(); _ = w.Shutdown(context.Background()) })
	w.MarkDirty("a")
	workerTick(t, ticks)
	workerReceive(t, entered)
	var writers sync.WaitGroup
	for i := 0; i < 32; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for j := 0; j < 32; j++ {
				if !w.MarkDirty("a") {
					t.Error("coalesced concurrent dirty rejected")
				}
			}
		}()
	}
	writers.Wait()
	for i := 1; i < MaxPendingOwners; i++ {
		w.MarkDirty(fmt.Sprint(i))
	}
	if w.MarkDirty("retry") {
		t.Fatal("overflow accepted")
	}
	unblock()
	workerTick(t, ticks) // batch now covers the 63 new owners and redirtied a
	workerTick(t, ticks) // all callbacks in the preceding batch have returned
	if !w.MarkDirty("retry") {
		t.Fatal("producer cannot retry after saturation clears")
	}
	if err := w.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(entered) != MaxPendingOwners+1 {
		t.Fatal("concurrent dirty was lost or duplicated", len(entered))
	}
}

func TestSealWorkerSecondShutdownCannotCancelDrain(t *testing.T) {
	entered, release := make(chan context.Context, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	w := newSealWorker(func(ctx context.Context, _ string) error { entered <- ctx; <-release; return ctx.Err() }, make(chan time.Time), func() {})
	t.Cleanup(func() { unblock(); _ = w.Shutdown(context.Background()) })
	w.MarkDirty("a")
	joined := make(chan error, 1)
	go func() { joined <- w.Shutdown(context.Background()) }()
	jobCtx := workerReceive(t, entered)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if jobCtx.Err() != nil {
		t.Fatal("second caller replaced active drain context")
	}
	unblock()
	if err := workerReceive(t, joined); err != nil {
		t.Fatal(err)
	}
}

func TestSealWorkerExpiredDrainNeverStartsIO(t *testing.T) {
	called := make(chan struct{}, 1)
	stopped := make(chan struct{})
	w := newSealWorker(func(context.Context, string) error { called <- struct{}{}; return nil }, make(chan time.Time), func() { close(stopped) })
	w.MarkDirty("a")
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := w.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := w.Wait(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	workerReceive(t, stopped)
	if len(called) != 0 {
		t.Fatal("expired drain started payload IO")
	}
}
