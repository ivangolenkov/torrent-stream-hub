package engine

import (
	"context"
	"errors"

	"github.com/anacrolix/torrent"
	"torrent-stream-hub/internal/models"
)

var ErrOperationConflict = errors.New("operation conflicts with torrent activity or metadata is unavailable")

func (e *Engine) StartRecheck(hash string) (*models.RecheckOperation, error) {
	e.admissionMu.Lock()
	defer e.admissionMu.Unlock()
	e.mu.RLock()
	mt := e.managedTorrents[hash]
	e.mu.RUnlock()
	if mt == nil {
		return nil, TorrentNotFoundError{Hash: hash}
	}
	return e.startRecheckLocked(mt)
}

func (e *Engine) startRecheckLocked(mt *ManagedTorrent) (*models.RecheckOperation, error) {
	if !e.currentTorrent(mt) {
		return nil, context.Canceled
	}
	active := e.streamManager.hasReaders(mt.hash)
	mt.mu.Lock()
	if mt.rechecking {
		if mt.verificationAttempt != nil && mt.verificationAttempt.operation != nil {
			copy := *mt.verificationAttempt.operation
			mt.mu.Unlock()
			return &copy, nil
		}
		mt.mu.Unlock()
		return nil, ErrOperationConflict
	}
	if mt.initializing || mt.t.Info() == nil || active {
		mt.mu.Unlock()
		return nil, ErrOperationConflict
	}
	e.nextRecheckID++
	operation := &models.RecheckOperation{ID: e.nextRecheckID, Status: "pending"}
	attempt := &verificationAttempt{operation: operation}
	if _, err := mt.applyVerificationEventLocked(verificationBegin); err != nil {
		mt.mu.Unlock()
		return nil, err
	}
	mt.recheckOperation = operation
	mt.verificationAttempt = attempt
	mt.mu.Unlock()
	mt.t.DisallowDataDownload()
	mt.t.DisallowDataUpload()
	for _, f := range mt.t.Files() {
		f.SetPriority(torrent.PiecePriorityNone)
	}
	mt.t.CancelPieces(0, mt.t.NumPieces())
	_ = e.streamManager.refreshTorrentOverlayLocked(mt)
	if err := e.persistVerificationLocked(mt, true, "Verification incomplete; successful local check required"); err != nil {
		// No verifier is run when the durable veto could not be established.
		// Keep the runtime veto and release the attempt through the same policy.
		e.completeVerificationLocked(mt, attempt, err)
		return nil, err
	}
	verify := e.verifyTorrent
	if verify == nil {
		verify = func(ctx context.Context, t *torrent.Torrent) error { return t.VerifyDataContext(ctx) }
	}
	if !e.StartTask(func(context.Context) {
		mt.mu.Lock()
		if mt.verificationAttempt != attempt || mt.ctx.Err() != nil {
			mt.mu.Unlock()
			// Close still needs to finalize the operation's cancellation history.
			_ = e.finishVerification(mt, attempt, context.Canceled)
			return
		}
		operation.Status = "running"
		mt.mu.Unlock()
		err := verify(mt.ctx, mt.t)
		e.finishVerification(mt, attempt, err)
	}) {
		e.completeVerificationLocked(mt, attempt, context.Canceled)
		return nil, context.Canceled
	}
	return &models.RecheckOperation{ID: operation.ID, Status: "pending"}, nil
}

func (e *Engine) finishVerification(mt *ManagedTorrent, attempt *verificationAttempt, err error) error {
	e.admissionMu.Lock()
	accepted, result := e.completeVerificationLocked(mt, attempt, err)
	e.admissionMu.Unlock()
	if accepted {
		e.manageResources()
	}
	return result
}

// admissionMu serializes validation, persistence and effects until A04 narrows
// this section. A duplicate completion must not persist against a later attempt.
func (e *Engine) completeVerificationLocked(mt *ManagedTorrent, attempt *verificationAttempt, err error) (bool, error) {
	if !e.currentTorrent(mt) {
		// Detached generations may finish their own operation history, never
		// persistence or gates. Close joins this task and exposes cancellation.
		mt.mu.Lock()
		if attempt != nil && mt.verificationAttempt == attempt {
			if attempt.operation != nil {
				attempt.operation.Status = "cancelled"
			}
			mt.verificationAttempt = nil
		}
		mt.mu.Unlock()
		return false, ErrOperationConflict
	}
	mt.mu.Lock()
	valid := attempt != nil && mt.verificationAttempt == attempt && mt.rechecking
	mt.mu.Unlock()
	if !valid {
		return false, ErrOperationConflict
	}
	if mt.ctx.Err() != nil {
		err = errors.Join(err, mt.ctx.Err())
	}
	if mt.ctx.Err() == nil {
		if err == nil {
			err = e.persistVerificationLocked(mt, false, "")
		} else {
			err = errors.Join(err, e.persistVerificationLocked(mt, true, "Verification failed; successful local check required"))
		}
	}
	mt.mu.Lock()
	operation := attempt.operation
	if operation != nil {
		operation.Status = "succeeded"
		if err != nil {
			operation.Status = "failed"
			operation.Error = err.Error()
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			operation.Status = "cancelled"
		}
	}
	event := verificationSucceeded
	if err != nil {
		event = verificationFailed
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		event = verificationCancelled
	}
	d, policyErr := mt.applyVerificationEventLocked(event)
	mt.verificationAttempt = nil
	mt.mu.Unlock()
	if policyErr != nil {
		return true, errors.Join(err, policyErr)
	}
	if d.closePayload {
		mt.t.DisallowDataDownload()
	}
	if d.closeUpload {
		mt.t.DisallowDataUpload()
	}
	if d.allowUpload && !e.cfg.BTNoUpload && mt.ctx.Err() == nil {
		mt.t.AllowDataUpload()
	}
	return true, err
}
