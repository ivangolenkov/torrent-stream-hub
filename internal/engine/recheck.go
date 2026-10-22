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
		if mt.recheckOperation != nil {
			copy := *mt.recheckOperation
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
	mt.recheckOperation = operation
	mt.rechecking = true
	mt.verificationUncertain = true
	mt.selectionComplete = false
	mt.downloadAllStarted = false
	mt.mu.Unlock()
	mt.t.DisallowDataDownload()
	mt.t.DisallowDataUpload()
	for _, f := range mt.t.Files() {
		f.SetPriority(torrent.PiecePriorityNone)
	}
	mt.t.CancelPieces(0, mt.t.NumPieces())
	_ = e.streamManager.refreshTorrentOverlayLocked(mt)
	verify := e.verifyTorrent
	if verify == nil {
		verify = func(ctx context.Context, t *torrent.Torrent) error { return t.VerifyDataContext(ctx) }
	}
	if !e.StartTask(func(context.Context) {
		mt.mu.Lock()
		operation.Status = "running"
		mt.mu.Unlock()
		err := verify(mt.ctx, mt.t)
		e.finishVerification(mt, operation, err)
	}) {
		mt.mu.Lock()
		mt.rechecking = false
		operation.Status = "cancelled"
		mt.mu.Unlock()
		return nil, context.Canceled
	}
	return &models.RecheckOperation{ID: operation.ID, Status: "pending"}, nil
}

func (e *Engine) finishVerification(mt *ManagedTorrent, operation *models.RecheckOperation, err error) {
	e.admissionMu.Lock()
	current := e.currentTorrent(mt)
	mt.mu.Lock()
	if operation != nil {
		operation.Status = "succeeded"
		if err != nil {
			operation.Status = "failed"
			operation.Error = err.Error()
		}
		if mt.ctx.Err() != nil {
			operation.Status = "cancelled"
		}
	}
	mt.rechecking = false
	mt.verificationUncertain = err != nil || mt.ctx.Err() != nil
	if current && mt.state == models.StateDownloading {
		mt.setStateLocked(models.StateQueued, models.ErrNone, "recheck completed; waiting for slot")
	}
	if current && err != nil && mt.ctx.Err() == nil && mt.state != models.StatePaused {
		mt.setStateLocked(models.StateError, models.ErrInvalidTorrent, "recheck failed")
	}
	state := mt.state
	mt.mu.Unlock()
	if current && state != models.StatePaused && state != models.StateError && !e.cfg.BTNoUpload {
		mt.t.AllowDataUpload()
	}
	e.admissionMu.Unlock()
	if current {
		e.manageResources()
	}
}
