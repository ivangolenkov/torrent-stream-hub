package engine

import (
	"github.com/anacrolix/torrent"
	"torrent-stream-hub/internal/models"
)

// admissionMu covers the accepted decision and all effects. Cancellation does
// not join tasks; library callbacks enqueue work rather than taking engine locks.
func (e *Engine) applyQuiesceLocked(mt *ManagedTorrent, d stateDecision, reason string) {
	e.clearBoostLocked(mt)
	if d.closePayload {
		mt.t.DisallowDataDownload()
	}
	if d.closeUpload {
		mt.t.DisallowDataUpload()
	}
	mt.mu.Lock()
	if mt.payloadCancel != nil {
		mt.payloadCancel(DownloadBlockedError{reason})
	}
	mt.mu.Unlock()
	if d.clearBasePriorities && mt.t.Info() != nil {
		for _, f := range mt.t.Files() {
			f.SetPriority(torrent.PiecePriorityNone)
		}
		mt.t.CancelPieces(0, mt.t.NumPieces())
	}
	if e.streamManager != nil {
		_ = e.streamManager.refreshTorrentOverlayLocked(mt)
	}
}

func (e *Engine) quiesceFaultLocked(mt *ManagedTorrent, kind stateEventKind) error {
	if !e.currentTorrent(mt) {
		return ErrOperationConflict
	}
	mt.mu.Lock()
	d, err := mt.applyStateEventLocked(stateEvent{kind: kind})
	mt.mu.Unlock()
	if err != nil {
		return err
	}
	e.applyQuiesceLocked(mt, d, string(kind))
	return e.persistSafetyFaultLocked(mt, kind)
}

// Safety keys are bounded by the finite producer set, not by fault count. The
// callback writes SQLite only; it must never acquire application/library locks.
func (e *Engine) SetSafetyPersistence(save func(models.RuntimeFence, models.ActivationBlockKind, string) error) {
	e.admissionMu.Lock()
	e.safetyPersistence = save
	e.admissionMu.Unlock()
}

func (e *Engine) persistSafetyFaultLocked(mt *ManagedTorrent, event stateEventKind) error {
	var kind models.ActivationBlockKind
	var reason string
	switch event {
	case stateMetadataFailed:
		kind, reason = models.BlockMetadataPersistence, string(models.ErrMetadataPersistence)
	case stateSelectionInvalid:
		kind, reason = models.BlockInvalidSelection, string(models.ErrInvalidSelection)
	case statePayloadMissing:
		kind, reason = models.BlockMissingFiles, string(models.ErrMissingFiles)
	default:
		return nil
	}
	mt.mu.Lock()
	if mt.safetyBlocks == nil {
		mt.safetyBlocks = make(map[models.ActivationBlockKind]string)
	}
	mt.safetyBlocks[kind] = reason
	f := mt.runtimeFence
	mt.mu.Unlock()
	// Before first binding there is no durable record capability. Binding must
	// carry these keys forward before it may acknowledge/observe that runtime.
	if f.Attachment == "" || e.safetyPersistence == nil {
		return nil
	}
	return e.safetyPersistence(f, kind, reason)
}

// RuntimeSafetyBlocks captures only the specified current runtime. The caller
// uses it on command failure to attach a newly committed staged Add before
// returning: a fault before first binding must not wait for periodic sync.
func (e *Engine) RuntimeSafetyBlocks(expected *torrent.Torrent, hash string) ([]models.ActivationBlock, error) {
	e.mu.RLock()
	mt := e.managedTorrents[hash]
	e.mu.RUnlock()
	if mt == nil || mt.t != expected {
		return nil, ErrOperationConflict
	}
	mt.mu.Lock()
	defer mt.mu.Unlock()
	blocks := make([]models.ActivationBlock, 0, len(mt.safetyBlocks))
	for kind, reason := range mt.safetyBlocks {
		blocks = append(blocks, models.ActivationBlock{Kind: kind, Reason: reason})
	}
	return blocks, nil
}
