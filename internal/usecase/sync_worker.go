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
	persistWorkers  int
	persistMu       sync.Mutex
	persistPending  map[string]persistenceJob
	persistWake     chan struct{}
	persistCtx      context.Context
	persistCancel   context.CancelFunc
	persistWG       sync.WaitGroup
	latestCompact   []byte
}

type PublishedSnapshot struct {
	Data    []byte
	Version string
	Compact []byte
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
		sw.persistCtx, sw.persistCancel = context.WithCancel(uc.engine.Context())
		sw.persistPending = make(map[string]persistenceJob)
		sw.persistWake = make(chan struct{}, 2)
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
	if len(sw.clients)+len(sw.snapshotClients) >= 128 {
		close(ch)
		return
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
		sw.syncOnceContext(ctx)
	}
}

func (sw *SyncWorker) syncOnce() { sw.syncOnceContext(sw.engine.Context()) }

func (sw *SyncWorker) syncOnceContext(ctx context.Context) {
	sw.syncMu.Lock()
	defer sw.syncMu.Unlock()
	// Allocate before capture, never after delayed JSON. A command receipt issued
	// in between must be able to retire this publication, including an empty list.
	version := sw.engine.NextSnapshotVersion()
	stateToSend, err := sw.captureSnapshotContext(ctx)
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
	compact, err := json.Marshal(CompactTorrents(stateToSend))
	if err != nil || len(compact) > 2<<20 || len(data) > 16<<20 {
		logging.Warnf("snapshot exceeds retained publication budget")
		return
	}
	sw.publishPair(data, compact, version)
	sw.enqueuePersistence(stateToSend)
}

// Returns owned DTOs: JSON encoding and mailbox publication run after releasing
// record handles. syncMu prevents concurrent Refresh from reordering them.
func (sw *SyncWorker) captureSnapshot() ([]*models.Torrent, error) {
	return sw.captureSnapshotContext(sw.engine.Context())
}

func (sw *SyncWorker) captureSnapshotContext(ctx context.Context) ([]*models.Torrent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// WAL readers never wait for record locks or observation commits. Projection
	// fences membership, record identity and desired/applied intent independently.
	stateToSend, err := readConsistentTorrents(func() (int64, error) { return sw.repo.ReadVersionContext(ctx) }, func() ([]*models.Torrent, error) { return sw.repo.GetAllTorrentsContext(ctx) }, sw.engine.GetAllTorrentsCached, func() func() bool {
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
		t.SnapshotFreshness = "sampled"
	}
	return stateToSend, nil
}

func (sw *SyncWorker) publish(data []byte) {
	sw.publishSnapshot(data, "")
}

func (sw *SyncWorker) publishSnapshot(data []byte, version string) {
	sw.publishPair(data, nil, version)
}

func (sw *SyncWorker) publishPair(data, compact []byte, version string) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	select {
	case <-sw.done:
		return
	default:
	}
	sw.latest = data
	sw.latestCompact = compact
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
		case ch <- PublishedSnapshot{Data: data, Version: version, Compact: compact}:
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

func (sw *SyncWorker) AddSnapshotClient(ch chan PublishedSnapshot) bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	select {
	case <-sw.done:
		close(ch)
		return false
	default:
	}
	if len(sw.snapshotClients)+len(sw.clients) >= 128 {
		close(ch)
		return false
	}
	sw.snapshotClients[ch] = true
	if sw.latest != nil {
		select {
		case ch <- PublishedSnapshot{Data: sw.latest, Version: sw.latestVersion, Compact: sw.latestCompact}:
		default:
		}
	}
	return true
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
		sw.persistMu.Lock()
		if sw.persistCancel != nil {
			sw.persistCancel()
		}
		sw.persistMu.Unlock()
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
	sw.persistWG.Wait()
}
