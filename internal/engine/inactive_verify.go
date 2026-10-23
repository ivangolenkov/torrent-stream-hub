package engine

import (
	"context"
	"fmt"

	"torrent-stream-hub/internal/models"
)

// Used under application record/root handles: no Resume/Delete or second inactive open
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
	if mt.policy.run != models.RunPaused || mt.policy.checking || mt.policy.terminal {
		mt.mu.Unlock()
		e.admissionMu.Unlock()
		return fmt.Errorf("inactive verification conflicts with current operation")
	}
	if _, err := mt.applyVerificationEventLocked(verificationBegin); err != nil {
		mt.mu.Unlock()
		e.admissionMu.Unlock()
		return err
	}
	attempt := &verificationAttempt{}
	mt.verificationAttempt = attempt
	mt.mu.Unlock()
	mt.t.DisallowDataDownload()
	mt.t.DisallowDataUpload()
	if err := e.persistVerificationLocked(mt, true, "Verification incomplete; successful local check required"); err != nil {
		e.completeVerificationLocked(mt, attempt, err)
		e.admissionMu.Unlock()
		return err
	}
	e.admissionMu.Unlock()
	verifyCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(mt.ctx, cancel)
	defer stop()
	err := mt.t.VerifyDataContext(verifyCtx)
	return e.finishVerification(mt, attempt, err)
}
