package usecase

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
)

type SyncWorker struct {
	engine    *engine.Engine
	repo      *repository.TorrentRepo
	clients   map[chan []byte]bool
	mu        sync.RWMutex
	latest    []byte
	done      chan struct{}
	closeOnce sync.Once
}

func NewSyncWorker(e *engine.Engine, r *repository.TorrentRepo) *SyncWorker {
	return &SyncWorker{
		engine:  e,
		repo:    r,
		clients: make(map[chan []byte]bool),
		done:    make(chan struct{}),
	}
}

func (sw *SyncWorker) AddClient(ch chan []byte) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	select {
	case <-sw.done:
		close(ch)
		return
	default:
	}
	sw.clients[ch] = true
	if sw.latest != nil {
		select {
		case ch <- sw.latest:
		default:
		}
	}
	logging.Debugf("sync worker client added clients=%d", len(sw.clients))
}

func (sw *SyncWorker) RemoveClient(ch chan []byte) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if sw.clients[ch] {
		delete(sw.clients, ch)
		close(ch)
	}
	logging.Debugf("sync worker client removed clients=%d", len(sw.clients))
}

func (sw *SyncWorker) Start() {
	sw.StartContext(context.Background())
}

func (sw *SyncWorker) StartContext(ctx context.Context) {
	defer sw.Close()
	logging.Infof("sync worker started interval=2s")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-sw.done:
			return
		case <-ticker.C:
		}
		sw.syncOnce()
	}
}

func (sw *SyncWorker) syncOnce() {
	// Take the snapshot under the same lock as add/delete/policy changes.
	// Never reinsert a DB-missing engine entry (including a failed add).
	sw.repo.Lifecycle.Lock()
	defer sw.repo.Lifecycle.Unlock()
	// Get all torrents from engine
	activeTorrents := sw.engine.GetAllTorrents()

	// Save state to DB
	for _, t := range activeTorrents {
		persisted, err := sw.repo.GetTorrent(t.Hash)
		if err != nil || persisted == nil {
			continue
		}
		if _, pending, err := sw.repo.CleanupPaths(t.Hash); err != nil || pending {
			continue
		}
		if err := sw.repo.SaveTorrent(t); err != nil {
			logging.Warnf("failed to sync torrent state to DB hash=%s: %v", t.Hash, err)
		}
	}

	// Include DB-only torrents and authoritative storage policy in the SSE snapshot.
	dbTorrents, err := sw.repo.GetAllTorrents()
	var stateToSend []*models.Torrent

	if err == nil {
		engineMap := make(map[string]*models.Torrent)
		for _, t := range activeTorrents {
			engineMap[t.Hash] = t
		}

		for _, dbT := range dbTorrents {
			if engT, ok := engineMap[dbT.Hash]; ok {
				mergePersistedMetadata(engT, dbT)
				// Use engine's updated stats with persisted compatibility metadata.
				stateToSend = append(stateToSend, engT)
			} else {
				stateToSend = append(stateToSend, dbT)
			}
		}
	} else {
		logging.Warnf("failed to load DB torrents for sync: %v", err)
		return // Never publish guessed policy or a falsely empty list on DB failure.
	}

	// Serialize to JSON
	for _, t := range stateToSend {
		t.SnapshotAt = time.Now().UnixNano()
	}
	data, err := json.Marshal(stateToSend)
	if err != nil {
		logging.Warnf("failed to marshal sync state: %v", err)
		return
	}

	// Broadcast to all SSE clients
	sw.publish(data)
}

func (sw *SyncWorker) publish(data []byte) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	select {
	case <-sw.done:
		return
	default:
	}
	sw.latest = data
	for ch := range sw.clients {
		// Drop queued snapshots: only the most recent state matters to a UI.
	drain:
		for {
			select {
			case <-ch:
			default:
				break drain
			}
		}
		select {
		case ch <- data:
		default:
			logging.Debugf("sync worker skipped slow SSE client")
		}
	}
}

func (sw *SyncWorker) Done() <-chan struct{} { return sw.done }

// Refresh publishes a consistent snapshot, including before the first periodic tick.
func (sw *SyncWorker) Refresh() { sw.syncOnce() }

func (sw *SyncWorker) Close() {
	sw.closeOnce.Do(func() {
		sw.mu.Lock()
		defer sw.mu.Unlock()
		close(sw.done)
		for ch := range sw.clients {
			delete(sw.clients, ch)
			close(ch)
		}
	})
}
