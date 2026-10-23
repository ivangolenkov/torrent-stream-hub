package engine

import (
	"github.com/anacrolix/torrent"
	"torrent-stream-hub/internal/models"
)

func (e *Engine) SetQueuePosition(hash string, position int64) {
	e.admissionMu.Lock()
	defer e.admissionMu.Unlock()
	e.mu.RLock()
	mt := e.managedTorrents[hash]
	e.mu.RUnlock()
	if mt != nil {
		mt.mu.Lock()
		mt.queuePosition = position
		mt.mu.Unlock()
	}
}

// admissionMu serializes selection completion with priority commands and slots.
func (e *Engine) reconcileSelectionLocked(mt *ManagedTorrent) {
	if !e.currentTorrent(mt) {
		return
	}
	info := mt.t.Info()
	if info == nil {
		return
	}
	prefix := verifiedPrefix(mt.t.PieceStateRuns(), info.PieceLength)
	mt.mu.Lock()
	if mt.initializing || mt.rechecking || mt.verificationUncertain {
		mt.mu.Unlock()
		return
	}
	selectionComplete, allComplete := true, true
	for i, file := range mt.t.Files() {
		complete := prefix(file.Offset()+file.Length())-prefix(file.Offset()) == file.Length()
		allComplete = allComplete && complete
		if mt.filePriorities[i] != models.PriorityNone {
			selectionComplete = selectionComplete && complete
		}
	}
	d, err := mt.applyStateEventLocked(stateEvent{kind: stateSelectionObserved, metadataKnown: true, selectedComplete: selectionComplete, payloadComplete: allComplete})
	mt.mu.Unlock()
	if err != nil {
		return
	}
	e.applyBackgroundReleaseLocked(mt, d)
}

// Does not close independent stream admission or restore all base priorities.
// admissionMu covers the state decision and these library/overlay effects.
func (e *Engine) applyBackgroundReleaseLocked(mt *ManagedTorrent, d stateDecision) {
	if d.clearBasePriorities {
		for _, file := range mt.t.Files() {
			file.SetPriority(torrent.PiecePriorityNone)
		}
		if d.closeBackgroundPayload && e.streamManager.ActiveStreamsForTorrent(mt.hash) == 0 {
			mt.t.DisallowDataDownload()
		}
		_ = e.streamManager.refreshTorrentOverlayLocked(mt)
	}
}
