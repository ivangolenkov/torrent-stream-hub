package engine

import (
	"context"
	"fmt"

	"torrent-stream-hub/internal/models"
)

// Used under application Lifecycle: no Resume/Delete or second inactive open
// may race the local verification. Engine task accounting protects storage.
func (e *Engine) VerifyInactive(ctx context.Context, hash string) error {
	if !e.beginWork() {
		return context.Canceled
	}
	defer e.wg.Done()
	e.admissionMu.Lock()
	e.mu.RLock()
	mt := e.managedTorrents[hash]
	e.mu.RUnlock()
	if mt == nil {
		e.admissionMu.Unlock()
		return TorrentNotFoundError{Hash: hash}
	}
	mt.mu.Lock()
	if mt.state != models.StatePaused || mt.rechecking {
		mt.mu.Unlock()
		e.admissionMu.Unlock()
		return fmt.Errorf("inactive verification conflicts with current operation")
	}
	mt.rechecking = true
	mt.verificationUncertain = true
	mt.mu.Unlock()
	mt.t.DisallowDataDownload()
	mt.t.DisallowDataUpload()
	e.admissionMu.Unlock()
	verifyCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(mt.ctx, cancel)
	defer stop()
	err := mt.t.VerifyDataContext(verifyCtx)
	e.admissionMu.Lock()
	mt.mu.Lock()
	mt.rechecking = false
	mt.verificationUncertain = err != nil
	mt.mu.Unlock()
	e.admissionMu.Unlock()
	return err
}
