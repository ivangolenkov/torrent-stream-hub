package engine

import (
	"context"
	"errors"
	"sync"
	"testing"

	"torrent-stream-hub/internal/config"
)

func TestTaskClassesAndEventQueueHaveIndependentBounds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{ctx: ctx, cancel: cancel, events: make(map[eventKey]func(context.Context)), eventWake: make(chan struct{}, 2)}
	for range 2 {
		if !e.startTaskClass("verification", 2, func(ctx context.Context) { <-ctx.Done() }) {
			t.Fatal("early rejection")
		}
	}
	if e.startTaskClass("verification", 2, func(context.Context) {}) {
		t.Fatal("unbounded verification")
	}
	if !e.startTaskClass("stream", 1, func(ctx context.Context) { <-ctx.Done() }) {
		t.Fatal("verification consumed stream budget")
	}
	for range 128 {
		if !e.queueEvent(&ManagedTorrent{}, "status", func(context.Context) {}) {
			t.Fatal("early event rejection")
		}
	}
	if e.queueEvent(&ManagedTorrent{}, "status", func(context.Context) {}) {
		t.Fatal("unbounded event queue")
	}
	for key := range e.events {
		if !e.queueEvent(key.owner, key.kind, func(context.Context) {}) {
			t.Fatal("coalesced event rejected")
		}
		break
	}
	e.BeginShutdown()
	e.wg.Wait()
	if e.StartTask(func(context.Context) {}) {
		t.Fatal("admitted after shutdown")
	}
}

func TestHTTPTransferBudgetAtomicAndReleaseIndependentOfDebounce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := &Engine{ctx: ctx, cancel: cancel, cfg: &config.Config{MaxActiveStreams: 1}}
	release, err := e.AcquireHTTPTransfer(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.AcquireHTTPTransfer(ctx, false); !errors.Is(err, ErrTransferLimit) {
				t.Errorf("over-admitted: %v", err)
			}
		}()
	}
	wg.Wait()
	head, err := e.AcquireHTTPTransfer(ctx, true)
	if err != nil {
		t.Fatal("HEAD consumed slot", err)
	}
	head()
	release()
	release()
	next, err := e.AcquireHTTPTransfer(ctx, false)
	if err != nil {
		t.Fatal("slot retained", err)
	}
	next()
	e.BeginShutdown()
	if _, err := e.AcquireHTTPTransfer(ctx, true); !errors.Is(err, context.Canceled) {
		t.Fatal("HEAD after shutdown", err)
	}
}
