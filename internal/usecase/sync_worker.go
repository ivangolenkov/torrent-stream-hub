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
	engine  *engine.Engine
	repo    *repository.TorrentRepo
	clients map[chan []byte]bool
	mu      sync.RWMutex
}

func NewSyncWorker(e *engine.Engine, r *repository.TorrentRepo) *SyncWorker {
	return &SyncWorker{
		engine:  e,
		repo:    r,
		clients: make(map[chan []byte]bool),
	}
}

func (sw *SyncWorker) AddClient(ch chan []byte) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.clients[ch] = true
	logging.Debugf("sync worker client added clients=%d", len(sw.clients))
}

func (sw *SyncWorker) RemoveClient(ch chan []byte) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	delete(sw.clients, ch)
	close(ch)
	logging.Debugf("sync worker client removed clients=%d", len(sw.clients))
}

func (sw *SyncWorker) Start() {
	sw.StartContext(context.Background())
}

func (sw *SyncWorker) StartContext(ctx context.Context) {
	logging.Infof("sync worker started interval=2s")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
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
	data, err := json.Marshal(stateToSend)
	if err != nil {
		logging.Warnf("failed to marshal sync state: %v", err)
		return
	}

	// Broadcast to all SSE clients
	sw.mu.RLock()
	for ch := range sw.clients {
		select {
		case ch <- data:
		default:
			logging.Debugf("sync worker skipped slow SSE client")
		}
	}
	sw.mu.RUnlock()
}
