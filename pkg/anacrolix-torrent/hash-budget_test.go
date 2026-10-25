package torrent

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func waitHashBudget(t *testing.T, b *HashBudget, n int) {
	t.Helper()
	deadline := time.After(time.Second)
	for b.Stats().Pending != n {
		select {
		case <-deadline:
			t.Fatalf("pending budget %+v, want %d", b.Stats(), n)
		default:
			runtime.Gosched()
		}
	}
}

func TestHashBudgetDevicesCancellationAndFairness(t *testing.T) {
	b := NewHashBudget(2, 1, 64, 8)
	release, err := b.Acquire(context.Background(), []string{"device-a"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := b.Acquire(context.Background(), []string{"device-b"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	failed := make(chan error, 1)
	go func() {
		done, err := b.Acquire(ctx, []string{"device-a"}, 2)
		if done != nil {
			done()
		}
		failed <- err
	}()
	waitHashBudget(t, b, 1)
	cancel()
	if <-failed == nil {
		t.Fatal("cancelled job ran")
	}
	if b.Stats().Active != 2 {
		t.Fatal("cancel released unrelated lease")
	}
	other()
	release()
	if b.Stats().Active != 0 {
		t.Fatal("leaked permit")
	}

	b = NewHashBudget(1, 1, 64, 8)
	release, _ = b.Acquire(context.Background(), []string{"disk"}, 0)
	type grant struct {
		name    string
		release func()
	}
	grants := make(chan grant, 20)
	go func() { r, _ := b.Acquire(context.Background(), []string{"disk"}, 0); grants <- grant{"background", r} }()
	waitHashBudget(t, b, 1)
	for i := 0; i < 12; i++ {
		go func() { r, _ := b.Acquire(context.Background(), []string{"disk"}, 2); grants <- grant{"reader", r} }()
	}
	waitHashBudget(t, b, 13)
	release()
	for i := 0; i < 13; i++ {
		select {
		case g := <-grants:
			if i < 8 && g.name != "reader" || i == 8 && g.name != "background" {
				t.Fatalf("fairness at %d: %s", i, g.name)
			}
			g.release()
		case <-time.After(time.Second):
			t.Fatal("budget starved")
		}
	}
}

func TestHashBudgetPendingBoundDoesNotLoseWork(t *testing.T) {
	b := NewHashBudget(1, 1, 2, 8)
	release, _ := b.Acquire(context.Background(), []string{"disk"}, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{}, 8)
	for i := 0; i < 8; i++ {
		go func() {
			r, err := b.Acquire(ctx, []string{"disk"}, 1)
			if err == nil {
				r()
			}
			done <- struct{}{}
		}()
	}
	waitHashBudget(t, b, 2)
	release()
	for i := 0; i < 8; i++ {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("saturation lost work")
		}
	}
	if s := b.Stats(); s.PeakPending > 2 || s.PeakActive > 1 || s.Active != 0 || s.Pending != 0 {
		t.Fatal(s)
	}
}
