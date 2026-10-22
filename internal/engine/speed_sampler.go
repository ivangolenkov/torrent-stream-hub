package engine

import (
	"context"
	"time"
)

func (e *Engine) speedSampler(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			e.sampleSpeeds(now)
		}
	}
}

func (e *Engine) sampleSpeeds(now time.Time) {
	e.admissionMu.Lock()
	defer e.admissionMu.Unlock()
	if e.ctx.Err() != nil {
		return
	}
	e.mu.RLock()
	mts := make([]*ManagedTorrent, 0, len(e.managedTorrents))
	for _, mt := range e.managedTorrents {
		mts = append(mts, mt)
	}
	e.mu.RUnlock()
	for _, mt := range mts {
		if mt == nil || mt.t == nil {
			continue
		}
		stats := mt.t.Stats()
		mt.mu.Lock()
		e.sampleSpeedsLocked(mt, stats, now)
		e.updatePeakDownloadSpeedLocked(mt, now)
		mt.mu.Unlock()
	}
}
