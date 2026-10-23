package engine

import (
	"errors"
	"fmt"

	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"

	"github.com/anacrolix/torrent"
)

func mapToPiecePriority(prio models.FilePriority) torrent.PiecePriority {
	switch prio {
	case models.PriorityNone:
		return torrent.PiecePriorityNone
	case models.PriorityHigh:
		return torrent.PiecePriorityHigh
	default:
		return torrent.PiecePriorityNormal
	}
}

func (e *Engine) applyFilePrioritiesAndDownload(mt *ManagedTorrent) (result error) {
	if mt.t.Info() == nil {
		return fmt.Errorf("metadata not ready")
	}

	mt.mu.Lock()
	var fault *stateDecision
	defer func() {
		mt.mu.Unlock()
		if fault != nil {
			e.applyQuiesceLocked(mt, *fault, "invalid file selection")
			result = errors.Join(result, e.persistSafetyFaultLocked(mt, stateSelectionInvalid))
			return
		}
		if e.streamManager != nil {
			_ = e.streamManager.refreshTorrentOverlayLocked(mt)
		}
	}()
	if mt.initializing {
		return fmt.Errorf("torrent initialization is not complete")
	}
	for index := range mt.filePriorities {
		if index < 0 || index >= len(mt.t.Files()) {
			d, err := mt.applyStateEventLocked(stateEvent{kind: stateSelectionInvalid})
			if err != nil {
				return err
			}
			fault = &d
			return fmt.Errorf("persisted file index out of bounds")
		}
	}

	for i := range mt.t.Files() {
		if _, ok := mt.filePriorities[i]; !ok {
			mt.filePriorities[i] = models.PriorityNormal
		}
	}

	for i, f := range mt.t.Files() {
		prio := mt.filePriorities[i]
		// Queued torrents may serve a reader/stream overlay without turning
		// their entire base selection into another background download.
		if !mt.policy.payloadEligible() || !mt.policy.slot && !mt.policy.payloadComplete {
			f.SetPriority(torrent.PiecePriorityNone)
			continue
		}
		f.SetPriority(mapToPiecePriority(prio))
	}
	return nil
}

func (e *Engine) SetFilePriority(hash string, fileIndex int, priority models.FilePriority) error {
	defer e.manageResources()
	e.admissionMu.Lock()
	defer e.admissionMu.Unlock()
	if e.ctx.Err() != nil {
		return e.ctx.Err()
	}
	if fileIndex < 0 || (priority != models.PriorityNormal && priority != models.PriorityHigh && priority != models.PriorityNone) {
		return fmt.Errorf("invalid file priority")
	}
	e.mu.RLock()
	mt, ok := e.managedTorrents[hash]
	e.mu.RUnlock()

	if !ok {
		logging.Debugf("set file priority requested for unmanaged torrent hash=%s", hash)
		return TorrentNotFoundError{Hash: hash}
	}

	if mt.t.Info() != nil {
		mt.mu.Lock()
		files := mt.t.Files()
		if fileIndex >= 0 && fileIndex < len(files) {
			mt.filePriorities[fileIndex] = priority
			mt.policyEpoch++
			logging.Debugf("set file priority hash=%s index=%d priority=%d", hash, fileIndex, priority)
		} else {
			mt.mu.Unlock()
			return fmt.Errorf("file index out of bounds")
		}
		mt.mu.Unlock()
		return e.applyFilePrioritiesAndDownload(mt)
	} else {
		mt.mu.Lock()
		mt.filePriorities[fileIndex] = priority
		mt.policyEpoch++
		mt.mu.Unlock()
	}

	return nil
}

func (e *Engine) SetTorrentFilesPriority(hash string, priority models.FilePriority) error {
	defer e.manageResources()
	e.admissionMu.Lock()
	defer e.admissionMu.Unlock()
	if e.ctx.Err() != nil {
		return e.ctx.Err()
	}
	if priority != models.PriorityNormal && priority != models.PriorityHigh && priority != models.PriorityNone {
		return fmt.Errorf("invalid file priority")
	}
	e.mu.RLock()
	mt, ok := e.managedTorrents[hash]
	e.mu.RUnlock()

	if !ok {
		logging.Debugf("set torrent files priority requested for unmanaged torrent hash=%s", hash)
		return TorrentNotFoundError{Hash: hash}
	}

	mt.mu.Lock()
	mt.policyEpoch++
	if mt.t.Info() != nil {
		for i := range mt.t.Files() {
			mt.filePriorities[i] = priority
		}
	}
	mt.mu.Unlock()

	if mt.t.Info() != nil {
		logging.Debugf("set torrent files priority hash=%s priority=%d", hash, priority)
		return e.applyFilePrioritiesAndDownload(mt)
	}
	return fmt.Errorf("metadata not ready")
}

// SetFilePriorities applies a committed bulk selection as one runtime command,
// including known file indices whose metadata is still pending.
func (e *Engine) SetFilePriorities(hash string, priorities map[int]models.FilePriority) error {
	defer e.manageResources()
	e.admissionMu.Lock()
	defer e.admissionMu.Unlock()
	if e.ctx.Err() != nil {
		return e.ctx.Err()
	}
	e.mu.RLock()
	mt := e.managedTorrents[hash]
	e.mu.RUnlock()
	if mt == nil {
		return TorrentNotFoundError{Hash: hash}
	}
	for index, priority := range priorities {
		if index < 0 || (mt.t.Info() != nil && index >= len(mt.t.Files())) || (priority != models.PriorityNormal && priority != models.PriorityHigh && priority != models.PriorityNone) {
			return fmt.Errorf("invalid file priority")
		}
	}
	mt.mu.Lock()
	mt.policyEpoch++
	for index, priority := range priorities {
		mt.filePriorities[index] = priority
	}
	mt.mu.Unlock()
	if mt.t.Info() != nil {
		return e.applyFilePrioritiesAndDownload(mt)
	}
	return nil
}
