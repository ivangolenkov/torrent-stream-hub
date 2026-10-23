package engine

import "torrent-stream-hub/internal/models"

func (sm *StreamManager) activityForTorrent(mt *ManagedTorrent) models.TorrentActivity {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	var a models.TorrentActivity
	for _, state := range sm.states {
		if state == nil || state.Torrent != mt {
			continue
		}
		for _, session := range state.Sessions {
			if session.Preload {
				a.Preloads++
			} else {
				a.Streams++
			}
		}
		a.Debouncing = a.Debouncing || state.DebounceTimer != nil
	}
	for _, handle := range sm.directStreams {
		if handle.torrent == mt {
			a.DirectReaders++
		}
	}
	a.Streaming = a.Streams > 0 || a.DirectReaders > 0
	return a
}
