package engine

import (
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

func (e *Engine) applyFilePrioritiesAndDownload(mt *ManagedTorrent) {
	if mt.t.Info() == nil {
		return
	}

	mt.mu.Lock()
	defer mt.mu.Unlock()

	for i := range mt.t.Files() {
		if _, ok := mt.filePriorities[i]; !ok {
			mt.filePriorities[i] = models.PriorityNormal
		}
	}

	for i, f := range mt.t.Files() {
		prio := mt.filePriorities[i]
		f.SetPriority(mapToPiecePriority(prio))
	}
}

func (e *Engine) SetFilePriority(hash string, fileIndex int, priority models.FilePriority) error {
	e.mu.RLock()
	mt, ok := e.managedTorrents[hash]
	e.mu.RUnlock()

	if !ok {
		logging.Debugf("set file priority requested for unmanaged torrent hash=%s", hash)
		return TorrentNotFoundError{Hash: hash}
	}

	mt.mu.Lock()
	mt.filePriorities[fileIndex] = priority
	mt.mu.Unlock()

	if mt.t.Info() != nil {
		mt.mu.Lock()
		files := mt.t.Files()
		if fileIndex >= 0 && fileIndex < len(files) {
			files[fileIndex].SetPriority(mapToPiecePriority(priority))
			logging.Debugf("set file priority hash=%s index=%d priority=%d", hash, fileIndex, priority)
		} else {
			mt.mu.Unlock()
			return fmt.Errorf("file index out of bounds")
		}
		mt.mu.Unlock()
	}

	return nil
}

func (e *Engine) SetTorrentFilesPriority(hash string, priority models.FilePriority) error {
	e.mu.RLock()
	mt, ok := e.managedTorrents[hash]
	e.mu.RUnlock()

	if !ok {
		logging.Debugf("set torrent files priority requested for unmanaged torrent hash=%s", hash)
		return TorrentNotFoundError{Hash: hash}
	}

	mt.mu.Lock()
	if mt.t.Info() != nil {
		for i := range mt.t.Files() {
			mt.filePriorities[i] = priority
		}
	} else {
		// If metadata is not ready, we can't easily iterate file indexes.
		// However, we can clear the map, and when metadata is ready we'll handle it?
		// Better yet, maybe we shouldn't allow SetTorrentFilesPriority before metadata is ready.
		// But in RestoreTorrents, we just use SetFilePriority.
	}
	mt.mu.Unlock()

	if mt.t.Info() != nil {
		mt.mu.Lock()
		for i, f := range mt.t.Files() {
			mt.filePriorities[i] = priority
			f.SetPriority(mapToPiecePriority(priority))
		}
		mt.mu.Unlock()
		logging.Debugf("set torrent files priority hash=%s priority=%d", hash, priority)
	} else {
		return fmt.Errorf("metadata not ready")
	}

	return nil
}
