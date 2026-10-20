package usecase

import (
	"context"
	"testing"
	"time"
)

func TestSyncWorkerStopsBeforeNextTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	worker := NewSyncWorker(nil, nil)
	go func() { defer close(done); worker.StartContext(ctx) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sync worker did not stop")
	}
}
