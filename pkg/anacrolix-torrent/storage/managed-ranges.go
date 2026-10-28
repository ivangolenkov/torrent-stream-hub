package storage

import (
	"sync"
	"time"
)

// Managed I/O excludes only conflicting byte ranges, not whole files. Entries
// exist only for active/waiting calls (not for every piece in the library).
// Earlier conflicting waiters take precedence; unrelated ranges bypass them.
// The mutex protects admission metadata only, never payload I/O or SQL.
type managedPayloadRanges struct {
	mu         sync.Mutex
	changed    *sync.Cond
	head, tail *managedPayloadRange
}

type managedPayloadRange struct {
	begin, end int64 // half-open torrent byte offsets, including shared-file boundaries
	write      bool
	next       *managedPayloadRange
}

func (c *managedPayloadRanges) lock(begin, end int64, write bool, waiting func()) *managedPayloadRange {
	r := &managedPayloadRange{begin: begin, end: end, write: write}
	c.mu.Lock()
	if c.tail == nil {
		c.head = r
	} else {
		c.tail.next = r
	}
	c.tail = r
	for {
		conflict := false
		for prev := c.head; prev != r; prev = prev.next {
			if prev.begin < r.end && r.begin < prev.end && (prev.write || r.write) {
				conflict = true
				break
			}
		}
		if !conflict {
			break
		}
		if waiting != nil {
			waiting()
			waiting = nil
		}
		if c.changed == nil {
			c.changed = sync.NewCond(&c.mu)
		}
		c.changed.Wait()
	}
	c.mu.Unlock()
	return r
}

func (c *managedPayloadRanges) unlock(r *managedPayloadRange) {
	c.mu.Lock()
	var prev *managedPayloadRange
	for p := c.head; p != r; p = p.next {
		prev = p
	}
	if prev == nil {
		c.head = r.next
	} else {
		prev.next = r.next
	}
	if c.tail == r {
		c.tail = prev
	}
	if c.changed != nil {
		c.changed.Broadcast()
	}
	c.mu.Unlock()
}

func (t *fileTorrentImpl) lockManagedPayloadRange(off, length int64, first, last int, write bool) func() {
	var start time.Time
	waiting := func() {
		if write && start.IsZero() {
			start = time.Now()
			t.ioTimings.writeWaitActive.Add(1)
		}
	}
	// Shared file leases are ONLY the Close/Reset join barrier. All managed
	// readers and writers share them. Keep the strict backend's exclusive file
	// leases unchanged: its mtime/stat accounting still requires serialization.
	for i := first; i <= last; i++ {
		if !t.files[i].payloadMu.TryRLock() {
			waiting()
			t.files[i].payloadMu.RLock()
		}
	}
	r := t.managedRanges.lock(off, off+length, write, waiting)
	if !start.IsZero() {
		ns := time.Since(start).Nanoseconds()
		t.ioTimings.writeWaitNS.Add(ns)
		recordIOMax(&t.ioTimings.writeWaitMaxNS, ns)
		t.ioTimings.writeWaitCount.Add(1)
		t.ioTimings.writeWaitActive.Add(-1)
	}
	return func() {
		t.managedRanges.unlock(r)
		for i := last; i >= first; i-- {
			t.files[i].payloadMu.RUnlock()
		}
	}
}
