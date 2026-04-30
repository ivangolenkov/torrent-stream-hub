package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

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

// AddStream increments the reference count for a file and enables sequential mode if it's the first stream.
func (sm *StreamManager) AddStream(ctx context.Context, hash string, fileIndex int) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	key := FileKey{Hash: hash, Index: fileIndex}
	state, exists := sm.states[key]

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
	}

	state.ActiveStreams++

	// If this is the first stream, enable sequential mode
	if state.ActiveStreams == 1 {
		if err := sm.setSequentialMode(hash, fileIndex, true); err != nil {
			state.ActiveStreams--
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

	if state.ActiveStreams <= 0 {
		state.ActiveStreams = 0
		// Start debounce timer
		if state.DebounceTimer != nil {
			state.DebounceTimer.Stop()
		}

		state.DebounceTimer = time.AfterFunc(DebounceDelay, func() {
			sm.mu.Lock()
			defer sm.mu.Unlock()

			// Verify if it's still 0 after delay
			st, ok := sm.states[key]
			if ok && st.ActiveStreams == 0 {
				_ = sm.setSequentialMode(hash, fileIndex, false)
				delete(sm.states, key)
			}
		})
	}
}

// setSequentialMode applies or removes sequential download priorities for the file
func (sm *StreamManager) setSequentialMode(hash string, fileIndex int, enable bool) error {
	sm.engine.mu.RLock()
	mt, ok := sm.engine.managedTorrents[hash]
	sm.engine.mu.RUnlock()

	if !ok {
		return fmt.Errorf("torrent not found: %s", hash)
	}

	files := mt.t.Files()
	if fileIndex < 0 || fileIndex >= len(files) {
		return fmt.Errorf("file index out of bounds: %d", fileIndex)
	}

	file := files[fileIndex]

	// If the file is already 100% downloaded, we don't need sequential mode
	if file.BytesCompleted() == file.Length() {
		enable = false

		// Update state to fully downloaded
		key := FileKey{Hash: hash, Index: fileIndex}
		if st, ok := sm.states[key]; ok {
			st.FullyDownloaded = true
		}
	}

	if enable {
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
		// Disable sequential/egoistic mode. Return to rarest-first/normal.
		for _, f := range files {
			f.SetPriority(torrent.PiecePriorityNormal)
		}
	}

	return nil
}
