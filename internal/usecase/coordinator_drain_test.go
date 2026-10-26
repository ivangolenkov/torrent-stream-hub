package usecase

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCoordinatorDrainWaitsNormalOwners(t *testing.T) {
	c := newApplicationCoordinator(context.Background())
	normal, err := c.acquire(context.Background(), "storage/a")
	if err != nil {
		t.Fatal(err)
	}
	defer normal.release()
	result := make(chan *coordinatorDrain, 1)
	go func() {
		d, err := c.beginDrain(context.Background())
		if err != nil {
			t.Error(err)
		}
		result <- d
	}()
	<-c.ctx.Done()
	if _, err := c.acquire(context.Background(), "other"); !errors.Is(err, context.Canceled) {
		t.Fatal("normal admission survived shutdown", err)
	}
	select {
	case <-result:
		t.Fatal("drain crossed existing reservation")
	default:
	}
	normal.release()
	var d *coordinatorDrain
	select {
	case d = <-result:
	case <-time.After(3 * time.Second):
		t.Fatal("drain never admitted")
	}
	if d == nil {
		t.Fatal("no drain capability")
	}
	release, err := d.acquireKeys(context.Background(), "root/a", "storage/a", "root/a")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.finish(); !errors.Is(err, errCoordinatorBusy) {
		t.Fatal("finished unjoined drain", err)
	}
	if _, err := c.beginDrain(context.Background()); !errors.Is(err, errCoordinatorBusy) {
		t.Fatal("second drain capability issued", err)
	}
	release()
	release()
	if err := d.finish(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.acquire(context.Background(), "late"); !errors.Is(err, context.Canceled) {
		t.Fatal("finished capability revived", err)
	}
	if _, err := c.acquire(context.Background(), "late"); !errors.Is(err, context.Canceled) {
		t.Fatal("normal commands revived", err)
	}
	if err := c.close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorDrainCancellationAndKeyExclusion(t *testing.T) {
	c := newApplicationCoordinator(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	d, err := c.beginDrain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, err := d.acquire(context.Background(), "shared")
	if err != nil {
		t.Fatal(err)
	}
	waiter := make(chan error, 1)
	go func() {
		release, err := d.acquireKeys(context.Background(), "independent", "shared")
		if release != nil {
			release()
		}
		waiter <- err
	}()
	cancel()
	select {
	case err := <-waiter:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled drain waiter retained keys")
	}
	first.release()
	if err := d.finish(); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refs != 0 || len(c.entries) != 0 {
		t.Fatal("drain leaked registry references", c.refs, len(c.entries))
	}
}

func TestCoordinatorExpiredDrainRetainsNormalReservation(t *testing.T) {
	c := newApplicationCoordinator(context.Background())
	normal, err := c.acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := c.beginDrain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	c.mu.Lock()
	refs := c.refs
	c.mu.Unlock()
	if refs != 1 {
		t.Fatal("deadline released an active reservation", refs)
	}
	normal.release()
	d, err := c.beginDrain(context.Background())
	if err != nil {
		t.Fatal("expired request consumed drain capability", err)
	}
	if err := d.finish(); err != nil {
		t.Fatal(err)
	}
}
