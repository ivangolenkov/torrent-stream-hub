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
	uc              *TorrentUseCase
	engine          *engine.Engine
	repo            *repository.TorrentRepo
	clients         map[chan []byte]bool
	mu              sync.RWMutex
	latest          []byte
	latestVersion   string
	snapshotClients map[chan PublishedSnapshot]bool
	done            chan struct{}
	closeOnce       sync.Once
	syncMu          sync.Mutex // Serializes sampler/publication, never application commands.
	marshal         func(any) ([]byte, error)
}

type PublishedSnapshot struct {
	Data    []byte
	Version string
}

func NewSyncWorker(uc *TorrentUseCase) *SyncWorker {
	sw := &SyncWorker{
		uc:              uc,
		clients:         make(map[chan []byte]bool),
		snapshotClients: make(map[chan PublishedSnapshot]bool),
		done:            make(chan struct{}),
	}
	if uc != nil {
		sw.engine, sw.repo = uc.engine, uc.repo
	}
	return sw
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
	sw.syncMu.Lock()
	defer sw.syncMu.Unlock()
	// Allocate before capture, never after delayed JSON. A command receipt issued
	// in between must be able to retire this publication, including an empty list.
	version := sw.engine.NextSnapshotVersion()
	stateToSend, err := sw.captureSnapshot()
	if err != nil {
		logging.Warnf("failed to load DB torrents for sync: %v", err)
		return
	}
	marshal := sw.marshal
	if marshal == nil {
		marshal = json.Marshal
	}
	data, err := marshal(stateToSend)
	if err != nil {
		logging.Warnf("failed to marshal sync state: %v", err)
		return
	}
	sw.publishSnapshot(data, version)
}

// Returns owned DTOs: JSON encoding and mailbox publication run after releasing
// record handles. syncMu prevents concurrent Refresh from reordering them.
func (sw *SyncWorker) captureSnapshot() ([]*models.Torrent, error) {
	// Persist each record under its command handles, then validate the cross-source
	// list. Never reinsert a DB-missing engine entry (including a failed Add).
	// Get all torrents from engine
	activeTorrents := sw.engine.GetAllTorrents()

	// Save state to DB
	for _, t := range activeTorrents {
		release, err := sw.uc.lockRecord(t.Hash)
		if err != nil {
			return nil, err
		}
		func() {
			defer release()
			persisted, err := sw.repo.GetTorrent(t.Hash)
			if err != nil || persisted == nil {
				return
			}
			if _, pending, err := sw.repo.CleanupPaths(t.Hash); err != nil || pending {
				return
			}
			_, err = capturePersistableRuntime(sw.engine, sw.repo, t.Hash)
			if err != nil {
				logging.Warnf("failed to sync torrent state to DB hash=%s: %v", t.Hash, err)
				// Failed persistence is not proof the runtime disappeared. Keep its
				// applied revision visible without relabelling it as the new intent.
				return
			}
		}()
	}

	// Include DB-only torrents and authoritative storage policy in the SSE snapshot.
	stateToSend, err := readConsistentTorrents(sw.repo.GetAllTorrents, sw.engine.GetAllTorrents, func() func() bool {
		stamp := sw.engine.CaptureReadStamp("")
		return func() bool { return sw.engine.ValidateReadStamp(stamp) }
	})
	if err != nil {
		return nil, err
	}
	// Timestamp remains a compatibility barrier. Stamp capture, not delayed
	// encoding/publication, so an old DTO cannot masquerade as post-command.
	for _, t := range stateToSend {
		t.SnapshotAt = time.Now().UnixNano()
	}
	return stateToSend, nil
}

func (sw *SyncWorker) publish(data []byte) {
	sw.publishSnapshot(data, "")
}

func (sw *SyncWorker) publishSnapshot(data []byte, version string) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	select {
	case <-sw.done:
		return
	default:
	}
	sw.latest = data
	sw.latestVersion = version
	for ch := range sw.snapshotClients {
	drainSnapshots:
		for {
			select {
			case <-ch:
			default:
				break drainSnapshots
			}
		}
		select {
		case ch <- PublishedSnapshot{Data: data, Version: version}:
		default:
		}
	}
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

func (sw *SyncWorker) AddSnapshotClient(ch chan PublishedSnapshot) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	select {
	case <-sw.done:
		close(ch)
		return
	default:
	}
	sw.snapshotClients[ch] = true
	if sw.latest != nil {
		select {
		case ch <- PublishedSnapshot{Data: sw.latest, Version: sw.latestVersion}:
		default:
		}
	}
}

func (sw *SyncWorker) RemoveSnapshotClient(ch chan PublishedSnapshot) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if sw.snapshotClients[ch] {
		delete(sw.snapshotClients, ch)
		close(ch)
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
		for ch := range sw.snapshotClients {
			delete(sw.snapshotClients, ch)
			close(ch)
		}
	})
}
