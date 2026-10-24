package engine

import (
	"context"
	"fmt"
	"time"
)

// Short callbacks share two workers and at most 128 pending keys. Repeated
// events replace the pending value, never create a goroutine per callback.
type eventKey struct {
	owner *ManagedTorrent
	kind  string
}

func (e *Engine) initEvents() {
	e.events = make(map[eventKey]func(context.Context))
	e.eventWake = make(chan struct{}, 2)
	for range 2 {
		e.StartTask(e.eventWorker)
	}
	e.StartTask(e.maintainPendingEffects)
}

func (e *Engine) queueEvent(owner *ManagedTorrent, kind string, fn func(context.Context)) bool {
	e.eventsMu.Lock()
	defer e.eventsMu.Unlock()
	if e.ctx.Err() != nil || e.events == nil {
		return false
	}
	key := eventKey{owner, kind}
	if _, exists := e.events[key]; !exists && len(e.events) >= 128 {
		return false
	}
	e.events[key] = fn
	select {
	case e.eventWake <- struct{}{}:
	default:
	}
	return true
}

func (e *Engine) eventWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.eventWake:
		}
		for ctx.Err() == nil {
			e.eventsMu.Lock()
			var next func(context.Context)
			for key, fn := range e.events {
				next = fn
				delete(e.events, key)
				break
			}
			e.eventsMu.Unlock()
			if next == nil {
				break
			}
			next(ctx)
		}
	}
}

func (e *Engine) queueDeferredRecheck(mt *ManagedTorrent) {
	e.queueEvent(mt, "deferred-recheck", func(context.Context) {
		if mt.ctx.Err() != nil || mt.t.Info() == nil {
			return
		}
		e.admissionMu.Lock()
		defer e.admissionMu.Unlock()
		if !e.currentTorrent(mt) {
			return
		}
		mt.mu.Lock()
		expected := mt.pendingRecheckBytes
		mt.mu.Unlock()
		if expected <= 0 {
			return
		}
		if mt.t.BytesCompleted() < expected {
			if _, err := e.startRecheckLocked(mt); err != nil {
				return
			}
		}
		mt.mu.Lock()
		if mt.pendingRecheckBytes == expected {
			mt.pendingRecheckBytes = 0
		}
		mt.mu.Unlock()
	})
}

// Safety work retains its source in the synchronous library latch, or in a
// generation-owned deadline. Saturation postpones persistence/cleanup, never
// reopens a failed writer or loses an accepted command.
func (e *Engine) maintainPendingEffects(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		e.mu.RLock()
		mts := make([]*ManagedTorrent, 0, len(e.managedTorrents))
		for _, mt := range e.managedTorrents {
			mts = append(mts, mt)
		}
		e.mu.RUnlock()
		for _, mt := range mts {
			mt.mu.Lock()
			boostExpired := !mt.boostUntil.IsZero() && !e.now().Before(mt.boostUntil)
			mt.mu.Unlock()
			if boostExpired {
				e.queueEvent(mt, "boost-expiry", func(context.Context) { e.expireBoost(mt) })
			}
			if mt.t.LastWriteChunkError() != nil {
				e.queueEvent(mt, "storage-error", func(context.Context) { e.payloadWriteError(mt, nil) })
			}
			mt.mu.Lock()
			pending := mt.pendingRecheckBytes > 0
			mt.mu.Unlock()
			if pending {
				e.queueDeferredRecheck(mt)
			}
		}
		sm := e.streamManager
		sm.mu.Lock()
		for key, state := range sm.states {
			if state.DebounceTimer == nil || time.Now().Before(state.debounceUntil) {
				continue
			}
			version := state.TimerVersion
			e.queueEvent(state.Torrent, fmt.Sprintf("debounce:%d", key.Index), func(context.Context) { sm.expireDebounce(key, state, version) })
		}
		sm.mu.Unlock()
	}
}
