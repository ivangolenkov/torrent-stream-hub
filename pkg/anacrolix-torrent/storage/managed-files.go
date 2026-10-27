package storage

import (
	"context"
	"errors"
	"os"
	"sync"
)

// Includes opening/closing slots. No overflow descriptors: when all slots are
// borrowed, admission waits cancelably. I/O never runs under the cache mutex.
const managedFileLimit = 16

type managedFileCache struct {
	mu      sync.Mutex
	entries map[int]*managedFileEntry
	changed chan struct{}
	clock   uint64
	err     error
}

type managedFileEntry struct {
	index                  int
	file                   *os.File
	info                   os.FileInfo // identity at acquisition, not live filesystem evidence
	writable, loading, bad bool
	refs                   int
	used                   uint64
}

func (c *managedFileCache) notify() {
	if c.changed != nil {
		close(c.changed)
		c.changed = nil
	}
}

type managedFileLease struct {
	owner *fileTorrentImpl
	entry *managedFileEntry
}

func (t *fileTorrentImpl) ioGuard() func(string) error {
	guard := t.client.opts.IOGuard
	if guard == nil || !t.managedPayload() {
		return guard
	}
	return func(path string) error {
		err := guard(path)
		if err == nil {
			return nil
		}
		// Relaxed detection is not permission to ignore a KNOWN failed volume
		// boundary. Fence future cache hits until this attachment is detached.
		c := &t.managedFiles
		c.mu.Lock()
		c.err = errors.Join(c.err, err)
		c.notify()
		c.mu.Unlock()
		t.freshMu.Lock()
		clear(t.fresh)
		clear(t.hashSnapshot)
		for _, sig := range t.signatures {
			sig.valid = false
			sig.complete = 0
			sig.published = false
		}
		t.freshMu.Unlock()
		t.checkpointEpoch.Add(1)
		return err
	}
}

func (t *fileTorrentImpl) acquireManagedFile(index int, write bool) (*managedFileLease, error) {
	c := &t.managedFiles
	ctx := t.client.opts.IOContext
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if t.readClosed.Load() || t.client.closed.Load() {
			return nil, os.ErrClosed
		}
		c.mu.Lock()
		if c.err != nil {
			err := c.err
			c.mu.Unlock()
			return nil, err
		}
		if c.entries == nil {
			c.entries = make(map[int]*managedFileEntry)
			c.notify()
		}
		e := c.entries[index]
		if e != nil && !e.loading && !e.bad && (!write || e.writable) {
			t.ioTimings.cacheHits.Add(1)
			c.clock++
			e.used = c.clock
			e.refs++
			c.mu.Unlock()
			return &managedFileLease{owner: t, entry: e}, nil
		}
		var victim *managedFileEntry
		wait := false
		if e != nil {
			if e.loading || e.refs != 0 || e.bad {
				wait = true
			} else {
				victim = e
			}
		} else if len(c.entries) >= managedFileLimit {
			for _, candidate := range c.entries {
				if candidate.loading || candidate.refs != 0 || candidate.bad {
					continue
				}
				if victim == nil || candidate.used < victim.used {
					victim = candidate
				}
			}
			wait = victim == nil
		}
		if wait {
			if c.changed == nil {
				c.changed = make(chan struct{})
			}
			changed := c.changed
			c.mu.Unlock()
			select {
			case <-changed:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue
		}
		if victim != nil {
			delete(c.entries, victim.index)
			t.ioTimings.cacheEvictions.Add(1)
		}
		t.ioTimings.cacheMisses.Add(1)
		e = &managedFileEntry{index: index, loading: true, writable: write}
		c.entries[index] = e // reserve capacity before closing/opening
		c.mu.Unlock()
		var closeErr error
		if victim != nil {
			closeErr = victim.file.Close()
			t.ioTimings.cacheOpen.Add(-1)
		}
		err := closeErr
		if err == nil {
			if write {
				start := t.ioTimings.writeGuard.begin()
				err = t.guardResumeFileWrite(index)
				t.ioTimings.writeGuard.end(start, err)
			}
			if err == nil {
				r, ok := t.io.(rootFileIo)
				if !ok {
					err = ErrCheckpointUnsupported
				} else {
					flags := os.O_RDONLY
					if write {
						flags = os.O_RDWR | os.O_CREATE
					}
					var timings *ioTimings
					if write {
						timings = &t.ioTimings
					}
					e.file, err = r.openMeasured(t.files[index].safeOsPath, flags, timings)
					if err == nil {
						t.ioTimings.cacheOpen.Add(1)
					}
				}
			}
			if err == nil {
				e.info, err = e.file.Stat()
			}
			if err == nil {
				t.freshMu.Lock()
				sig := t.signatures[index]
				changed := sig != nil && sig.valid && !os.SameFile(sig.FileInfo, e.info)
				t.freshMu.Unlock()
				if changed {
					err = errors.Join(ErrCheckpointChanged, t.revokeResumeFile(index))
				}
			}
		}
		if err != nil && e.file != nil {
			cleanupErr := e.file.Close()
			t.ioTimings.cacheOpen.Add(-1)
			closeErr = errors.Join(closeErr, cleanupErr)
			err = errors.Join(err, cleanupErr)
		}
		c.mu.Lock()
		if err != nil {
			delete(c.entries, index)
		} else {
			c.clock++
			e.used = c.clock
			e.loading = false
			e.refs = 1
		}
		// A delayed close error must remain observable at backend shutdown too.
		c.err = errors.Join(c.err, closeErr)
		c.notify()
		c.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return &managedFileLease{owner: t, entry: e}, nil
	}
}

func (l *managedFileLease) ReadAt(p []byte, off int64) (int, error) {
	if l.owner.client.closed.Load() {
		return 0, os.ErrClosed
	}
	if ctx := l.owner.client.opts.IOContext; ctx != nil && ctx.Err() != nil {
		return 0, ctx.Err()
	}
	n, err := l.entry.file.ReadAt(p, off)
	if observe := l.owner.client.opts.IORead; observe != nil && n > 0 {
		observe(l.owner.files[l.entry.index].safeOsPath, int64(n))
	}
	if err != nil {
		l.invalidate()
	}
	return n, err
}

func (l *managedFileLease) invalidate() {
	c := &l.owner.managedFiles
	c.mu.Lock()
	l.entry.bad = true
	c.mu.Unlock()
}

func (l *managedFileLease) Close() error {
	c := &l.owner.managedFiles
	c.mu.Lock()
	e := l.entry
	e.refs--
	closeFile := e.bad && e.refs == 0
	if closeFile {
		e.loading = true
	}
	c.notify()
	c.mu.Unlock()
	if !closeFile {
		return nil
	}
	err := e.file.Close()
	l.owner.ioTimings.cacheOpen.Add(-1)
	c.mu.Lock()
	delete(c.entries, e.index)
	c.err = errors.Join(c.err, err)
	c.notify()
	c.mu.Unlock()
	return err
}

// Caller owns lifecycle exclusively AND all payload leases, joining readers as
// well as writers/hash publication/sync. Never close a borrowed descriptor.
func (c *managedFileCache) closeAll(timings *ioTimings) error {
	c.mu.Lock()
	for _, e := range c.entries {
		if e.loading || e.refs != 0 {
			c.mu.Unlock()
			return ErrCheckpointChanged
		}
	}
	entries := c.entries
	c.entries = nil
	err := c.err
	c.notify()
	c.mu.Unlock()
	for _, e := range entries {
		err = errors.Join(err, e.file.Close())
		timings.cacheOpen.Add(-1)
	}
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
	return err
}

func (t *fileTorrentImpl) resetManagedPayload(ctx context.Context) error {
	if err := t.checkpointLock(ctx); err != nil {
		return err
	}
	defer t.lifecycle.Unlock()
	for i := range t.files {
		t.files[i].payloadMu.Lock()
	}
	defer func() {
		for i := len(t.files) - 1; i >= 0; i-- {
			t.files[i].payloadMu.Unlock()
		}
	}()
	t.freshMu.Lock()
	clear(t.fresh)
	clear(t.hashSnapshot)
	for _, sig := range t.signatures {
		sig.valid = false
		sig.complete = 0
		sig.published = false
	}
	t.freshMu.Unlock()
	t.checkpointEpoch.Add(1)
	// Failed durable revocation must not keep the live read gate open. Do not
	// start hashing if the persistent barrier failed; a later retry still sees
	// the pending mask and must revoke it successfully.
	return errors.Join(t.revokeResume(0, t.info.NumPieces()), t.managedFiles.closeAll(&t.ioTimings))
}
