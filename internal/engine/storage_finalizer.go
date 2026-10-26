package engine

import (
	"context"
	"time"

	"torrent-stream-hub/internal/logging"
)

const storageDrainBudget = 10 * time.Second

// StorageFinalizer owns background storage work not tied to the runtime context.
// Shutdown stops admission and requests drain; Wait MUST join issued operations,
// even if Shutdown's context has expired. It must not reenter Engine.Close or wait
// for an engine task that itself waits for this finalizer.
type StorageFinalizer interface {
	Shutdown(context.Context) error
	Wait() error
}

// RegisterStorageFinalizer transfers one worker's shutdown ownership to Engine.
// False transfers nothing: the caller must stop/join its worker itself. The single
// slot is reserved for the checkpoint producer; it is not a generic cleanup queue.
// Producers must stop on engine cancellation and tolerate rejected enqueue. This
// registration grants neither coordinator ownership nor permission to publish a
// checkpoint; the adapter must still enforce all mutation/attachment fences.
func (e *Engine) RegisterStorageFinalizer(finalizer StorageFinalizer) bool {
	e.storageTaskMu.Lock()
	defer e.storageTaskMu.Unlock()
	if finalizer == nil || e.ctx.Err() != nil || e.storageFinalizer != nil {
		return false
	}
	e.storageFinalizer = finalizer
	return true
}

func (e *Engine) beginStorageFinalization() func() {
	e.storageTaskMu.Lock()
	finalizer := e.storageFinalizer
	e.storageTaskMu.Unlock()
	if finalizer == nil {
		return func() {}
	}
	// BeginShutdown already cancelled e.ctx. Using it here would turn every
	// orderly seal into an immediately cancelled job. Deadline bounds drain,
	// never the lifetime of an already issued syscall/resource lease.
	ctx, cancel := context.WithTimeout(context.Background(), storageDrainBudget)
	// Request stop/drain before joining ordinary tasks: a coordinator cleanup
	// task can itself be waiting for a reservation held by an active seal. Its
	// callback must receive the shutdown budget even while that join is pending.
	drained := make(chan error, 1)
	go func() { drained <- finalizer.Shutdown(ctx) }()
	return func() {
		defer cancel()
		if err := <-drained; err != nil {
			logging.Warnf("storage checkpoint drain incomplete: %v", err)
		}
		if err := finalizer.Wait(); err != nil {
			logging.Warnf("storage checkpoint worker joined with error: %v", err)
		}
	}
}
