package engine

import (
	"context"
	"errors"
	"sync"
)

var ErrTransferLimit = errors.New("HTTP transfer limit reached")
var ErrTaskLimit = errors.New("background task limit reached")

// Reserve before record locks or disk preparation. HEAD owns a record lease but
// neither a payload slot nor playback debounce. Preload has its own budget.
func (e *Engine) AcquireHTTPTransfer(ctx context.Context, head bool) (func(), error) {
	e.storageTaskMu.Lock()
	defer e.storageTaskMu.Unlock()
	if e.ctx.Err() != nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if head {
		return func() {}, nil
	}
	if e.httpTransfers >= e.cfg.MaxActiveStreams {
		return nil, ErrTransferLimit
	}
	e.httpTransfers++
	var once sync.Once
	return func() { once.Do(func() { e.storageTaskMu.Lock(); e.httpTransfers--; e.storageTaskMu.Unlock() }) }, nil
}
