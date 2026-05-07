package engine

import (
	"context"
	"fmt"
	"sync"
	"time"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"

	"github.com/anacrolix/torrent"
)

const (
	DebounceDelay = 10 * time.Second
)

type FileKey struct {
	Hash  string
	Index int
}

type StreamState struct {
	ActiveStreams int
	DebounceTimer *time.Timer
	// Track when it's fully downloaded so we can remove sequential mode
	FullyDownloaded bool
}

type StreamManager struct {
	mu     sync.Mutex
	engine *Engine
	states map[FileKey]*StreamState
}

func NewStreamManager(e *Engine) *StreamManager {
	return &StreamManager{
		engine: e,
		states: make(map[FileKey]*StreamState),
	}
}

func (sm *StreamManager) ActiveStreamsForTorrent(hash string) int {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	active := 0
	for key, state := range sm.states {
		if key.Hash != hash || state == nil {
			continue
		}
		if state.ActiveStreams > 0 {
			active += state.ActiveStreams
			continue
		}
		if state.DebounceTimer != nil {
			active++
		}
	}
	return active
}

func (sm *StreamManager) ActiveStreamsTotal() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	active := 0
	for _, state := range sm.states {
		if state == nil {
			continue
		}
		if state.ActiveStreams > 0 {
			active += state.ActiveStreams
			continue
		}
		if state.DebounceTimer != nil {
			active++
		}
	}
	return active
}

// AddStream increments the reference count for a file and enables sequential mode if it's the first stream.
func (sm *StreamManager) AddStream(ctx context.Context, hash string, fileIndex int) error {
	sm.mu.Lock()

	key := FileKey{Hash: hash, Index: fileIndex}
	state, exists := sm.states[key]
	logging.Infof("stream add requested hash=%s file_index=%d existing=%t", hash, fileIndex, exists)

	if !exists {
		state = &StreamState{
			ActiveStreams: 0,
		}
		sm.states[key] = state
	}

	// Cancel existing debounce timer if any (e.g. user seeked within 10s)
	if state.DebounceTimer != nil {
		state.DebounceTimer.Stop()
		state.DebounceTimer = nil
		logging.Debugf("stream debounce cancelled hash=%s file_index=%d", hash, fileIndex)
	}

	state.ActiveStreams++
	logging.Debugf("stream reference incremented hash=%s file_index=%d active=%d", hash, fileIndex, state.ActiveStreams)

	// If this is the first stream, enable sequential mode
	enableSequential := state.ActiveStreams == 1
	sm.mu.Unlock()

	if enableSequential {
		if err := sm.setSequentialMode(hash, fileIndex, true); err != nil {
			sm.mu.Lock()
			if st, ok := sm.states[key]; ok {
				st.ActiveStreams--
				if st.ActiveStreams <= 0 {
					delete(sm.states, key)
				}
			}
			sm.mu.Unlock()
			logging.Warnf("failed to enable sequential mode hash=%s file_index=%d: %v", hash, fileIndex, err)
			return err
		}
	}

	// Watch for context cancellation to remove the stream
	go func() {
		<-ctx.Done()
		sm.RemoveStream(hash, fileIndex)
	}()

	return nil
}

// RemoveStream decrements the reference count. If it hits 0, starts the debounce timer.
func (sm *StreamManager) RemoveStream(hash string, fileIndex int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	key := FileKey{Hash: hash, Index: fileIndex}
	state, exists := sm.states[key]
	if !exists {
		return
	}

	state.ActiveStreams--
	logging.Debugf("stream reference decremented hash=%s file_index=%d active=%d", hash, fileIndex, state.ActiveStreams)

	if state.ActiveStreams <= 0 {
		state.ActiveStreams = 0
		// Start debounce timer
		if state.DebounceTimer != nil {
			state.DebounceTimer.Stop()
		}

		state.DebounceTimer = time.AfterFunc(DebounceDelay, func() {
			sm.mu.Lock()

			// Verify if it's still 0 after delay
			st, ok := sm.states[key]
			if ok && st.ActiveStreams == 0 {
				delete(sm.states, key)
				sm.mu.Unlock()
				if err := sm.setSequentialMode(hash, fileIndex, false); err != nil {
					logging.Warnf("failed to disable sequential mode hash=%s file_index=%d: %v", hash, fileIndex, err)
				}
				logging.Infof("stream debounce elapsed hash=%s file_index=%d sequential_disabled=%t", hash, fileIndex, true)
				return
			}
			sm.mu.Unlock()
		})
		logging.Debugf("stream debounce scheduled hash=%s file_index=%d delay=%s", hash, fileIndex, DebounceDelay)
	}
}

// setSequentialMode applies or removes sequential download priorities for the file
func (sm *StreamManager) setSequentialMode(hash string, fileIndex int, enable bool) error {
	sm.engine.mu.RLock()
	mt, ok := sm.engine.managedTorrents[hash]
	sm.engine.mu.RUnlock()

	if !ok {
		logging.Debugf("sequential mode requested for missing torrent hash=%s file_index=%d enable=%t", hash, fileIndex, enable)
		return TorrentNotFoundError{Hash: hash}
	}
	if mt.t.Info() == nil {
		logging.Debugf("sequential mode requested before metadata hash=%s file_index=%d enable=%t", hash, fileIndex, enable)
		return fmt.Errorf("torrent metadata is not available yet")
	}

	files := mt.t.Files()
	if fileIndex < 0 || fileIndex >= len(files) {
		logging.Debugf("sequential mode file index out of bounds hash=%s file_index=%d files=%d enable=%t", hash, fileIndex, len(files), enable)
		return fmt.Errorf("file index out of bounds: %d", fileIndex)
	}

	file := files[fileIndex]

	// If the file is already 100% downloaded, we don't need sequential mode
	if file.BytesCompleted() == file.Length() {
		enable = false
		logging.Debugf("sequential mode skipped for completed file hash=%s file_index=%d", hash, fileIndex)

		// Update state to fully downloaded
		key := FileKey{Hash: hash, Index: fileIndex}
		if st, ok := sm.states[key]; ok {
			st.FullyDownloaded = true
		}
	}

	if enable {
		logging.Infof("sequential mode enabled hash=%s file_index=%d file=%q", hash, fileIndex, file.DisplayPath())
		// Set priorites:
		// We want to download the file sequentially.
		// First, let's make sure it's downloading.
		file.Download()

		// Set high priority to the first few and last pieces for MOOV atoms (metadata for mp4)
		// anacrolix/torrent provides Download() which does sequential if configured, but by default it's rarest-first.
		// anacrolix/torrent doesn't have a strict "Sequential" mode per file out-of-the-box in the high-level API.
		// Wait, Reader supports sequential read.
		// If we use mt.t.NewReader(), it will prioritize pieces automatically based on reads.
		// However, we need to instruct the engine to prioritize this file.
		// file.SetPriority(torrent.PiecePriorityHigh) will prioritize the whole file over others.
		// Let's set priority High for the whole file, and Normal for others.
		for i, f := range files {
			if i == fileIndex {
				f.SetPriority(torrent.PiecePriorityHigh)
			} else {
				// Don't stop other files completely, just normal priority, or none if we want strict QoS.
				// TЗ: "Если счетчик перешел от 0 к 1, движок включает эгоистичный режим (Sequential mode)...
				// подавление фоновых загрузок в пользу стрима".
				if f.Priority() == torrent.PiecePriorityHigh {
					f.SetPriority(torrent.PiecePriorityNormal)
				}
			}
		}
	} else {
		logging.Infof("sequential mode disabled hash=%s file_index=%d", hash, fileIndex)
		// Disable sequential/egoistic mode. Return to rarest-first/normal.
		sm.engine.mu.RLock()
		state := mt.state
		sm.engine.mu.RUnlock()

		if state == models.StatePaused || state == models.StateError || state == models.StateMissingFiles || state == models.StateDiskFull {
			logging.Debugf("torrent is in inactive state %s, skipping restore to normal priority hash=%s", state, hash)
			for _, f := range files {
				f.SetPriority(torrent.PiecePriorityNone)
			}
			mt.t.CancelPieces(0, mt.t.NumPieces())
		} else {
			sm.engine.applyFilePrioritiesAndDownload(mt)
		}
	}

	return nil
}
