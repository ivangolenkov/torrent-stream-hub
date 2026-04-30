package usecase

import (
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
	logging.Infof("sync worker started interval=2s")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		// Get all torrents from engine
		activeTorrents := sw.engine.GetAllTorrents()

		// Save state to DB
		for _, t := range activeTorrents {
			if err := sw.repo.SaveTorrent(t); err != nil {
				logging.Warnf("failed to sync torrent state to DB hash=%s: %v", t.Hash, err)
			}
		}

		// Also get ALL torrents from DB to send complete state to UI
		// Since engine might not have paused torrents in memory after restart (we haven't implemented restore yet)
		// But let's assume for now UI gets state from Engine + DB.
		// A proper way is merging DB list with Engine list.
		dbTorrents, err := sw.repo.GetAllTorrents()
		var stateToSend []*models.Torrent

		if err == nil {
			engineMap := make(map[string]*models.Torrent)
			for _, t := range activeTorrents {
				engineMap[t.Hash] = t
			}

			for _, dbT := range dbTorrents {
				if engT, ok := engineMap[dbT.Hash]; ok {
					// Use engine's updated stats
					stateToSend = append(stateToSend, engT)
				} else {
					stateToSend = append(stateToSend, dbT)
				}
			}
		} else {
			logging.Warnf("failed to load DB torrents for sync: %v", err)
			stateToSend = activeTorrents
		}

		// Serialize to JSON
		data, err := json.Marshal(stateToSend)
		if err != nil {
			logging.Warnf("failed to marshal sync state: %v", err)
			continue
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
}
