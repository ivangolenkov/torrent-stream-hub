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
	mt.selectionComplete = selectionComplete
	eligible := mt.state == models.StateDownloading || mt.state == models.StateQueued || mt.state == models.StateSeeding
	changed := false
	if eligible && mt.downloadBlock == "" {
		if allComplete {
			mt.setStateLocked(models.StateSeeding, models.ErrNone, "verified payload complete")
		} else if selectionComplete || mt.state == models.StateSeeding {
			changed = mt.state != models.StateQueued
			mt.setStateLocked(models.StateQueued, models.ErrNone, "selection complete or payload invalidated")
			mt.downloadAllStarted = false
		}
	}
	mt.mu.Unlock()
	if changed {
		for _, file := range mt.t.Files() {
			file.SetPriority(torrent.PiecePriorityNone)
		}
		if e.streamManager.ActiveStreamsForTorrent(mt.hash) == 0 {
			mt.t.DisallowDataDownload()
		}
		_ = e.streamManager.refreshTorrentOverlayLocked(mt)
	}
}
