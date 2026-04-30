package usecase

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"torrent-stream-hub/internal/engine"
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
}

func (sw *SyncWorker) RemoveClient(ch chan []byte) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	delete(sw.clients, ch)
	close(ch)
}

func (sw *SyncWorker) Start() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		// Get all torrents from engine
		activeTorrents := sw.engine.GetAllTorrents()

		// Save state to DB
		for _, t := range activeTorrents {
			if err := sw.repo.SaveTorrent(t); err != nil {
				log.Printf("Failed to sync torrent state to DB: %v", err)
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
			stateToSend = activeTorrents
		}

		// Serialize to JSON
		data, err := json.Marshal(stateToSend)
		if err != nil {
			continue
		}

		// Broadcast to all SSE clients
		sw.mu.RLock()
		for ch := range sw.clients {
			select {
			case ch <- data:
			default:
				// Client channel full, skip
			}
		}
		sw.mu.RUnlock()
	}
}
