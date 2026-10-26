package usecase

import (
	"context"
	"errors"
	"sort"
	"sync"
)

var errCoordinatorBusy = errors.New("record command is already reserved")

// applicationCoordinator is independent of repository and runtime existence.
// Registry.mu owns references only; it is NEVER held while waiting for a record,
// doing SQLite work, or executing filesystem/library effects. Waiting references
// prevent deletion/recreation of a key's lock underneath an existing waiter.
//
// All command/sync/cleanup/migration consumers share this registry. Namespace,
// eviction and lease protocols keep their separate reservations; independent keys
// never imply independent access to the same physical root.
type applicationCoordinator struct {
	mu          sync.Mutex
	entries     map[string]*commandEntry
	ctx         context.Context
	cancel      context.CancelFunc
	closed      bool
	refs        int
	drained     chan struct{}
	drainClosed bool
	drainOwner  *coordinatorDrain
}

type commandEntry struct {
	permit chan struct{}
	refs   int
}

type commandReservation struct {
	owner *applicationCoordinator
	key   string
	entry *commandEntry
	once  sync.Once
}

func newApplicationCoordinator(parent context.Context) *applicationCoordinator {
	ctx, cancel := context.WithCancel(parent)
	return &applicationCoordinator{entries: make(map[string]*commandEntry), ctx: ctx, cancel: cancel, drained: make(chan struct{})}
}

func (c *applicationCoordinator) reference(key string) (*commandEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.ctx.Err() != nil {
		return nil, context.Canceled
	}
	e := c.entries[key]
	if e == nil {
		e = &commandEntry{permit: make(chan struct{}, 1)}
		e.permit <- struct{}{}
		c.entries[key] = e
	}
	e.refs++
	c.refs++
	return e, nil
}

func (c *applicationCoordinator) unreference(key string, e *commandEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.refs--
	c.refs--
	if e.refs == 0 {
		delete(c.entries, key)
	}
	if c.closed && c.refs == 0 && !c.drainClosed {
		c.drainClosed = true
		close(c.drained)
	}
}

func (c *applicationCoordinator) acquire(ctx context.Context, key string) (*commandReservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e, err := c.reference(key)
	if err != nil {
		return nil, err
	}
	return c.acquireReferenced(ctx, key, e)
}

func (c *applicationCoordinator) acquireReferenced(ctx context.Context, key string, e *commandEntry) (*commandReservation, error) {
	select {
	case <-ctx.Done():
		c.unreference(key, e)
		return nil, ctx.Err()
	case <-c.ctx.Done():
		c.unreference(key, e)
		return nil, context.Canceled
	case <-e.permit:
	}
	// A ready permit and cancellation can race in select. Do not admit a command
	// merely because the runtime chose the permit branch first.
	if err := ctx.Err(); err != nil {
		e.permit <- struct{}{}
		c.unreference(key, e)
		return nil, err
	}
	if c.ctx.Err() != nil {
		e.permit <- struct{}{}
		c.unreference(key, e)
		return nil, context.Canceled
	}
	return &commandReservation{owner: c, key: key, entry: e}, nil
}

// tryAcquire is for eviction/reservation re-evaluation: callers must never wait
// for a second object's command lock while retaining an unordered first lock.
func (c *applicationCoordinator) tryAcquire(key string) (*commandReservation, error) {
	e, err := c.reference(key)
	if err != nil {
		return nil, err
	}
	select {
	case <-e.permit:
		if c.ctx.Err() != nil {
			e.permit <- struct{}{}
			c.unreference(key, e)
			return nil, context.Canceled
		}
		return &commandReservation{owner: c, key: key, entry: e}, nil
	default:
		c.unreference(key, e)
		return nil, errCoordinatorBusy
	}
}

func (r *commandReservation) release() {
	r.once.Do(func() { r.entry.permit <- struct{}{}; r.owner.unreference(r.key, r.entry) })
}

// acquireKeys orders canonical/alias keys before waiting. Cancellation releases
// the accepted prefix in reverse order; no partial namespace lock set escapes.
func (c *applicationCoordinator) acquireKeys(ctx context.Context, keys ...string) (func(), error) {
	keys = append([]string(nil), keys...)
	sort.Strings(keys)
	var reservations []*commandReservation
	release := func() {
		for i := len(reservations) - 1; i >= 0; i-- {
			reservations[i].release()
		}
	}
	for i, key := range keys {
		if i > 0 && key == keys[i-1] {
			continue
		}
		r, err := c.acquire(ctx, key)
		if err != nil {
			release()
			return nil, err
		}
		reservations = append(reservations, r)
	}
	var once sync.Once
	return func() { once.Do(release) }, nil
}

// close closes command admission and cancels waiters before joining references.
// The caller waits with no coordinator/record/engine locks. Deadline limits the
// caller's wait, not an arbitrary synchronous syscall owned by an active command.
func (c *applicationCoordinator) close(ctx context.Context) error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.cancel()
		if c.refs == 0 && !c.drainClosed {
			c.drainClosed = true
			close(c.drained)
		}
	}
	c.mu.Unlock()
	select {
	case <-c.drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// coordinatorDrain is a shutdown-only capability on the SAME key registry. It
// is issued only after normal admission has stopped and all its references have
// joined. It cannot revive normal commands or be reused across coordinators.
type coordinatorDrain struct {
	owner  *applicationCoordinator
	ctx    context.Context
	closed bool // owner.mu
}

func (c *applicationCoordinator) beginDrain(ctx context.Context) (*coordinatorDrain, error) {
	if err := c.close(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.drainOwner != nil {
		return nil, errCoordinatorBusy
	}
	d := &coordinatorDrain{owner: c, ctx: ctx}
	c.drainOwner = d
	return d, nil
}

func (d *coordinatorDrain) acquire(ctx context.Context, key string) (*commandReservation, error) {
	c := d.owner
	c.mu.Lock()
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	if err := d.ctx.Err(); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	if d.closed || c.drainOwner != d {
		c.mu.Unlock()
		return nil, context.Canceled
	}
	e := c.entries[key]
	if e == nil {
		e = &commandEntry{permit: make(chan struct{}, 1)}
		e.permit <- struct{}{}
		c.entries[key] = e
	}
	e.refs++
	c.refs++
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		c.unreference(key, e)
		return nil, ctx.Err()
	case <-d.ctx.Done():
		c.unreference(key, e)
		return nil, d.ctx.Err()
	case <-e.permit:
	}
	c.mu.Lock()
	valid := !d.closed && c.drainOwner == d && ctx.Err() == nil && d.ctx.Err() == nil
	c.mu.Unlock()
	if !valid {
		e.permit <- struct{}{}
		c.unreference(key, e)
		return nil, context.Canceled
	}
	return &commandReservation{owner: c, key: key, entry: e}, nil
}

func (d *coordinatorDrain) acquireKeys(ctx context.Context, keys ...string) (func(), error) {
	keys = append([]string(nil), keys...)
	sort.Strings(keys)
	var reservations []*commandReservation
	var once sync.Once
	release := func() {
		once.Do(func() {
			for i := len(reservations) - 1; i >= 0; i-- {
				reservations[i].release()
			}
		})
	}
	for i, key := range keys {
		if i > 0 && keys[i-1] == key {
			continue
		}
		r, err := d.acquire(ctx, key)
		if err != nil {
			release()
			return nil, err
		}
		reservations = append(reservations, r)
	}
	return release, nil
}

// finish is not an operation join. The finalizer must first join the worker and
// release all its reservations; finish then destroys admission permanently.
func (d *coordinatorDrain) finish() error {
	c := d.owner
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refs != 0 {
		return errCoordinatorBusy
	}
	d.closed = true
	return nil
}
