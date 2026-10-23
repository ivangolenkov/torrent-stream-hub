package usecase

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func coordinatorCounts(c *applicationCoordinator) (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries), c.refs
}

func TestCoordinatorReferenceLifetimeAndCancellation(t *testing.T) {
	c := newApplicationCoordinator(context.Background())
	defer c.close(context.Background())
	first, err := c.acquire(context.Background(), "db-only")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Register the waiter explicitly so the interleaving does not depend on the
	// goroutine reaching an unobservable semaphore wait before first.release.
	entry, err := c.reference("db-only")
	if err != nil || entry != first.entry {
		t.Fatal("two locks for one key", err)
	}
	first.release()
	third, err := c.acquire(context.Background(), "db-only")
	if err != nil || third.entry != entry {
		t.Fatal("waiter lost lock identity", err)
	}
	result := make(chan error, 1)
	go func() {
		r, err := c.acquireReferenced(ctx, "db-only", entry)
		if r != nil {
			r.release()
		}
		result <- err
	}()
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	third.release()
	third.release()
	first.release()
	if keys, refs := coordinatorCounts(c); keys != 0 || refs != 0 {
		t.Fatal("cancelled waiter leaked registry", keys, refs)
	}
}

func TestCoordinatorIndependentKeysAndBusyEviction(t *testing.T) {
	c := newApplicationCoordinator(context.Background())
	defer c.close(context.Background())
	a, err := c.acquire(context.Background(), "A")
	if err != nil {
		t.Fatal(err)
	}
	defer a.release()
	if r, err := c.tryAcquire("A"); !errors.Is(err, errCoordinatorBusy) || r != nil {
		t.Fatal("eviction acquired active command", err)
	}
	result := make(chan error, 1)
	go func() {
		b, err := c.acquire(context.Background(), "B")
		if b != nil {
			b.release()
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("B waited on A")
	}
}

func TestCoordinatorCanonicalKeyOrderingAndCancelledPrefix(t *testing.T) {
	c := newApplicationCoordinator(context.Background())
	defer c.close(context.Background())
	b, err := c.acquire(context.Background(), "B")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if release, err := c.acquireKeys(ctx, "B", "A", "A"); !errors.Is(err, context.Canceled) || release != nil {
		t.Fatal(err)
	}
	b.release()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			keys := []string{"B", "A", "A"}
			if i%2 == 0 {
				keys = []string{"A", "B"}
			}
			release, err := c.acquireKeys(context.Background(), keys...)
			if err != nil {
				t.Error(err)
				return
			}
			release()
			release()
		}(i)
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("opposite alias order deadlocked")
	}
	if keys, refs := coordinatorCounts(c); keys != 0 || refs != 0 {
		t.Fatal(keys, refs)
	}
}

func TestCoordinatorCloseCancelsWaitersAndJoinsActiveHandles(t *testing.T) {
	c := newApplicationCoordinator(context.Background())
	a, err := c.acquire(context.Background(), "A")
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan error, 1)
	entry, err := c.reference("A")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		r, err := c.acquireReferenced(context.Background(), "A", entry)
		if r != nil {
			r.release()
		}
		waiting <- err
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("close failed to retain active owner", err)
	}
	if err := <-waiting; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if r, err := c.acquire(context.Background(), "B"); !errors.Is(err, context.Canceled) || r != nil {
		t.Fatal("late command admitted", err)
	}
	a.release()
	if err := c.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if keys, refs := coordinatorCounts(c); keys != 0 || refs != 0 {
		t.Fatal(keys, refs)
	}
}
