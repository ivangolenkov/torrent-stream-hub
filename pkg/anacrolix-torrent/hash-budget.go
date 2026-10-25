package torrent

import (
	"context"
	"slices"
	"sync"
)

// HashBudget shares physical verification capacity across Clients. Acquire is
// called outside the client lock. Keys describe live filesystem identities,
// not paths. Existing client/per-torrent hasher limits bound the callers too.
type HashBudget struct {
	mu                                            sync.Mutex
	global, perResource, pendingLimit, burstLimit int
	active, burst                                 int
	resources                                     map[string]int
	pending                                       []*hashBudgetWaiter
	changed                                       chan struct{}
	peakActive, peakPending                       int
}

type hashBudgetWaiter struct {
	keys     []string
	priority int
	ready    chan struct{}
	granted  bool
}

type HashBudgetStats struct{ Active, Pending, PeakActive, PeakPending int }

func NewHashBudget(global, perResource, pendingLimit, burstLimit int) *HashBudget {
	if global < 1 || perResource < 1 || pendingLimit < 1 || burstLimit < 1 {
		panic("invalid hash budget")
	}
	return &HashBudget{global: global, perResource: perResource, pendingLimit: pendingLimit, burstLimit: burstLimit, resources: make(map[string]int), changed: make(chan struct{})}
}

func (b *HashBudget) Stats() HashBudgetStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return HashBudgetStats{b.active, len(b.pending), b.peakActive, b.peakPending}
}

func (b *HashBudget) Acquire(ctx context.Context, keys []string, priority int) (func(), error) {
	keys = append([]string(nil), keys...)
	if len(keys) == 0 {
		keys = []string{"unknown"}
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	w := &hashBudgetWaiter{keys: keys, priority: priority, ready: make(chan struct{})}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b.mu.Lock()
		if len(b.pending) < b.pendingLimit {
			break
		}
		changed := b.changed
		b.mu.Unlock()
		// Saturation keeps the mandatory piece queued in its existing bounded
		// hasher slot; it does not report a failed hash or create another task.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
	b.pending = append(b.pending, w)
	b.peakPending = max(b.peakPending, len(b.pending))
	b.scheduleLocked()
	b.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if w.granted {
				b.active--
				for _, key := range w.keys {
					b.resources[key]--
					if b.resources[key] == 0 {
						delete(b.resources, key)
					}
				}
			} else {
				b.pending = slices.DeleteFunc(b.pending, func(other *hashBudgetWaiter) bool { return other == w })
			}
			b.scheduleLocked()
			b.changedLocked()
		})
	}
	select {
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	case <-w.ready:
		if err := ctx.Err(); err != nil {
			release()
			return nil, err
		}
		return release, nil
	}
}

func (b *HashBudget) changedLocked() { close(b.changed); b.changed = make(chan struct{}) }

func (b *HashBudget) scheduleLocked() {
	for b.active < b.global {
		best := -1
		for i, w := range b.pending {
			eligible := true
			for _, key := range w.keys {
				if b.resources[key] >= b.perResource {
					eligible = false
					break
				}
			}
			if !eligible {
				continue
			}
			if best < 0 || b.burst < b.burstLimit && w.priority > b.pending[best].priority {
				best = i
			}
		}
		if best < 0 {
			return
		}
		w := b.pending[best]
		b.pending = slices.Delete(b.pending, best, best+1)
		if b.burst >= b.burstLimit || w.priority == 0 {
			b.burst = 0
		} else {
			b.burst++
		}
		b.active++
		b.peakActive = max(b.peakActive, b.active)
		for _, key := range w.keys {
			b.resources[key]++
		}
		w.granted = true
		close(w.ready)
		b.changedLocked()
	}
}
