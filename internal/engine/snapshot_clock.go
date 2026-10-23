package engine

import (
	"crypto/rand"
	"strconv"
	"sync"
	"sync/atomic"
)

type snapshotClock struct {
	once     sync.Once
	epoch    string
	sequence atomic.Uint64
}

// The epoch is process/Engine scoped, not a record ID or wall-clock timestamp.
// Sequence is transmitted as decimal text; JavaScript must not round it.
func (e *Engine) NextSnapshotVersion() string {
	e.snapshot.once.Do(func() { e.snapshot.epoch = rand.Text() })
	return e.snapshot.epoch + "/" + strconv.FormatUint(e.snapshot.sequence.Add(1), 10)
}
