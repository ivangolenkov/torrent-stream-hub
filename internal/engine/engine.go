package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

var ErrTorrentNotFound = errors.New("torrent not found")

type TorrentNotFoundError struct {
	Hash string
}

func (e TorrentNotFoundError) Error() string {
	return fmt.Sprintf("torrent not found: %s", e.Hash)
}

func (e TorrentNotFoundError) Is(target error) bool {
	return target == ErrTorrentNotFound
}

type Engine struct {
	storageIOObserver func(operation, path string, bytes int64)
	client            *torrent.Client
	storage           storage.ClientImplCloser
	cfg               *config.Config

	ctx                   context.Context
	cancel                context.CancelFunc
	wg                    sync.WaitGroup
	closeOnce             sync.Once
	storageTaskMu         sync.Mutex
	httpTransfers         int
	taskCounts            map[string]int
	eventsMu              sync.Mutex
	events                map[eventKey]func(context.Context)
	eventWake             chan struct{}
	storageRootMu         sync.RWMutex
	storageRootResolver   func(string) (string, error)
	storageLayoutResolver func(string, *metainfo.Info) (payload.Layout, error)
	storageIOGuard        func(string) error
	storageAliasRegistrar func(string, string) error

	mu                      sync.RWMutex
	admissionMu             sync.Mutex
	diskProbe               func(string) (uint64, error)
	saveDownloadBlock       func(string, string) error
	verifyTorrent           func(context.Context, *torrent.Torrent) error
	verificationPersistence func(models.RuntimeFence, bool, string) error
	safetyPersistence       func(models.RuntimeFence, models.ActivationBlockKind, string) error
	nextRecheckID           uint64
	clock                   func() time.Time
	managedTorrents         map[string]*ManagedTorrent
	detaching               map[string]*ManagedTorrent // admissionMu: namespace reserved until Drop/closers finish
	registering             map[string]*ManagedTorrent // unpublished staged generations, admissionMu
	registerMetainfo        func(string, *metainfo.MetaInfo) error
	dropTorrent             func(*torrent.Torrent) // optional controlled Drop for deterministic tests
	// Independent generation registry for callbacks under the library lock.
	statusTorrents            sync.Map
	resourceLogMu             sync.Mutex
	lastResourceLog           time.Time
	lastResourceKey           string
	invalidMetainfoCount      int
	pieceCompletionBackend    string
	pieceCompletionPersistent bool
	pieceCompletionErr        string
	pieceCompletion           storage.PieceCompletion

	streamManager *StreamManager
	snapshot      snapshotClock
}

type ManagedTorrent struct {
	verificationSampleOperation uint64
	verificationSampleBytes     int64
	verificationProgressAtMS    int64
	hash                        string
	mu                          sync.Mutex
	t                           *torrent.Torrent
	state                       models.TorrentState
	err                         models.ErrorReason
	metadataLogged              bool
	metadataWaitLogged          bool
	downloadAllStarted          bool
	lastStatsAt                 time.Time
	lastRawReadBytes            int64
	lastDataReadBytes           int64
	lastReadBytes               int64
	lastWrittenBytes            int64
	rawDownloadSpeed            int64
	dataDownloadSpeed           int64
	downloadSpeed               int64
	uploadSpeed                 int64
	lastPeerSummary             models.PeerSummary
	lastPeerLog                 time.Time
	trackerStatus               string
	trackerError                string
	sourceURI                   string
	metainfo                    *metainfo.MetaInfo
	addedAt                     time.Time
	degraded                    bool
	lastSwarmCheckAt            time.Time
	lastSwarmRefreshAt          time.Time
	lastSwarmRefreshReason      string
	lastPeerRefreshAt           time.Time
	lastPeerRefreshReason       string
	lastHealthyAt               time.Time
	stallStartedAt              time.Time
	peakDownloadSpeed           int64
	peakUpdatedAt               time.Time
	boostUntil                  time.Time
	normalMaxEstablishedConns   int
	filePriorities              map[int]models.FilePriority
	// Restored torrents remain download-disabled until settings and persistence
	// have been installed. Readers cannot override the library-level prohibition.
	initializing           bool
	downloadBlock          string
	downloadBlockPersisted bool
	payloadCtx             context.Context
	payloadCancel          context.CancelCauseFunc
	ctx                    context.Context
	cancel                 context.CancelFunc
	rechecking             bool
	queuePosition          int64
	selectionComplete      bool
	recheckOperation       *models.RecheckOperation
	verificationUncertain  bool
	runtimeFence           models.RuntimeFence
	observationSequence    int64
	appliedIntentRevision  int64
	verificationAttempt    *verificationAttempt
	intentApplyFailure     *models.IntentApplyFailure
	terminal               bool
	policy                 runtimePolicy
	policyEpoch            uint64
	slotToken              uint64
	preparation            *payloadPreparation
	healthProbe            *payloadPreparation
	pendingRecheckBytes    int64
	safetyBlocks           map[models.ActivationBlockKind]string
	expectedPayload        map[int]bool
}

type Option func(*Engine)

// WithStorageIOObserver instruments payload storage without polling or an extra
// hash pass. The callback may run concurrently and must not call the engine.
func WithStorageIOObserver(observe func(operation, path string, bytes int64)) Option {
	return func(e *Engine) { e.storageIOObserver = observe }
}

// WithPieceCompletion transfers backend ownership to the engine's storage.
// The application owns the database and closes it after Engine.Close.
func WithPieceCompletion(pc storage.PieceCompletion) Option {
	return func(e *Engine) { e.pieceCompletion = pc }
}

func New(cfg *config.Config, options ...Option) (*Engine, error) {
	config.ApplyDefaults(cfg)
	if err := cfg.ValidateResources(); err != nil {
		return nil, err
	}
	logging.Infof("initializing torrent engine download_dir=%s torrent_port=%d max_downloads=%d min_free_space_gb=%d bt_seed=%t bt_no_upload=%t bt_profile=%s bt_download_profile=%s bt_retrackers=%s dht=%t pex=%t upnp=%t tcp=%t utp=%t ipv6=%t established_conns=%d half_open=%d total_half_open=%d peers_low=%d peers_high=%d dial_rate=%d swarm_watchdog=%t swarm_check_sec=%d swarm_refresh_cooldown_sec=%d swarm_min_peers=%d swarm_min_seeds=%d swarm_stalled_speed=%d swarm_stalled_duration_sec=%d swarm_boost_conns=%d swarm_boost_duration_sec=%d peer_drop_ratio=%.2f seed_drop_ratio=%.2f speed_drop_ratio=%.2f peak_ttl_sec=%d",
		cfg.DownloadDir,
		cfg.TorrentPort,
		cfg.MaxActiveDownloads,
		cfg.MinFreeSpaceGB,
		cfg.BTSeed,
		cfg.BTNoUpload,
		cfg.BTClientProfile,
		cfg.BTDownloadProfile,
		cfg.BTRetrackersMode,
		!cfg.BTDisableDHT,
		!cfg.BTDisablePEX,
		!cfg.BTDisableUPNP,
		!cfg.BTDisableTCP,
		!cfg.BTDisableUTP,
		!cfg.BTDisableIPv6,
		cfg.BTEstablishedConns,
		cfg.BTHalfOpenConns,
		cfg.BTTotalHalfOpen,
		cfg.BTPeersLowWater,
		cfg.BTPeersHighWater,
		cfg.BTDialRateLimit,
		cfg.BTSwarmWatchdogEnabled,
		cfg.BTSwarmCheckIntervalSec,
		cfg.BTSwarmRefreshCooldownSec,
		cfg.BTSwarmMinConnectedPeers,
		cfg.BTSwarmMinConnectedSeeds,
		cfg.BTSwarmStalledSpeedBps,
		cfg.BTSwarmStalledDurationSec,
		cfg.BTSwarmBoostConns,
		cfg.BTSwarmBoostDurationSec,
		cfg.BTSwarmPeerDropRatio,
		cfg.BTSwarmSeedDropRatio,
		cfg.BTSwarmSpeedDropRatio,
		cfg.BTSwarmPeakTTLSec,
	)

	ctx, cancel := context.WithCancel(context.Background())
	eng := &Engine{
		cfg:             cfg,
		ctx:             ctx,
		cancel:          cancel,
		managedTorrents: make(map[string]*ManagedTorrent),
	}
	for _, option := range options {
		option(eng)
	}
	client, torrentStorage, err := eng.newTorrentClient()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create torrent client: %w", err)
	}
	eng.client = client
	eng.storage = torrentStorage
	eng.streamManager = NewStreamManager(eng)
	eng.initEvents()

	eng.startBackground(eng.resourceMonitor)
	eng.startBackground(eng.speedSampler)
	if cfg.BTSwarmWatchdogEnabled {
		eng.startBackground(eng.swarmRefreshMonitor)
	}

	return eng, nil
}

func (e *Engine) startBackground(fn func(context.Context)) {
	e.StartTask(fn)
}

// StartTask admits service-owned work atomically against Close.
func (e *Engine) StartTask(fn func(context.Context)) bool { return e.startTaskClass("service", 64, fn) }

func (e *Engine) startTaskClass(class string, limit int, fn func(context.Context)) bool {
	e.storageTaskMu.Lock()
	if e.ctx == nil || e.ctx.Err() != nil {
		e.storageTaskMu.Unlock()
		return false
	}
	if e.taskCounts == nil {
		e.taskCounts = make(map[string]int)
	}
	if e.taskCounts[class] >= limit {
		e.storageTaskMu.Unlock()
		return false
	}
	e.taskCounts[class]++
	e.wg.Add(1)
	e.storageTaskMu.Unlock()
	go func() {
		defer e.wg.Done()
		defer func() { e.storageTaskMu.Lock(); e.taskCounts[class]--; e.storageTaskMu.Unlock() }()
		fn(e.ctx)
	}()
	return true
}

func (e *Engine) GenerationContext(hash string) (context.Context, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	mt := e.managedTorrents[hash]
	if mt == nil {
		return nil, TorrentNotFoundError{Hash: hash}
	}
	return mt.ctx, nil
}

func (e *Engine) Context() context.Context { return e.ctx }

func (e *Engine) beginWork() bool {
	e.storageTaskMu.Lock()
	defer e.storageTaskMu.Unlock()
	if e.ctx == nil || e.ctx.Err() != nil {
		return false
	}
	e.wg.Add(1)
	return true
}

func (e *Engine) currentTorrent(mt *ManagedTorrent) bool {
	if mt == nil || e.ctx == nil || e.ctx.Err() != nil {
		return false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.managedTorrents[mt.hash] == mt
}

func (e *Engine) StreamManager() *StreamManager {
	return e.streamManager
}

func (e *Engine) DownloadDir() string {
	return e.cfg.DownloadDir
}

func (e *Engine) ConfigDir() string {
	if strings.TrimSpace(e.cfg.DBPath) == "" {
		return e.cfg.DownloadDir
	}
	return filepath.Dir(e.cfg.DBPath)
}

func (e *Engine) GetTorrentFile(hash string, index int) (*torrent.File, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	mt, ok := e.managedTorrents[hash]
	if !ok {
		logging.Debugf("get torrent file missing torrent hash=%s file_index=%d", hash, index)
		return nil, TorrentNotFoundError{Hash: hash}
	}

	if mt.t.Info() == nil {
		logging.Debugf("get torrent file before metadata hash=%s file_index=%d", hash, index)
		return nil, fmt.Errorf("torrent metadata is not available yet")
	}

	files := mt.t.Files()
	if index < 0 || index >= len(files) {
		logging.Debugf("get torrent file index out of bounds hash=%s file_index=%d files=%d", hash, index, len(files))
		return nil, fmt.Errorf("file index out of bounds")
	}

	return files[index], nil
}

func (e *Engine) GetTorrent(hash string) *models.Torrent {
	e.mu.RLock()
	mt, ok := e.managedTorrents[hash]
	e.mu.RUnlock()

	if !ok {
		return nil
	}
	mt.t.RefreshStorageCompletion()
	return e.mapManagedTorrent(mt)
}

// GetTorrentCached is an observational API read. Admission/open paths refresh
// evidence separately; API polling must not stat a complete catalog.
func (e *Engine) GetTorrentCached(hash string) *models.Torrent {
	e.mu.RLock()
	mt := e.managedTorrents[hash]
	e.mu.RUnlock()
	if mt == nil {
		return nil
	}
	return e.mapManagedTorrent(mt)
}

func (e *Engine) Warmup(ctx context.Context, hash string, index int, size int64) (int64, int64, error) {
	file, err := e.GetTorrentFile(hash, index)
	if err != nil {
		return 0, 0, err
	}

	target := size
	if target <= 0 {
		target = defaultStreamHeadPrebuffer
	}
	if target > file.Length() {
		target = file.Length()
	}

	// If file is already completed and verified on disk, fast path returns immediately
	if ready, _, checkErr := e.CheckFileDirectReady(hash, index); checkErr == nil && ready {
		logging.Debugf("warmup skipped for completed disk file hash=%s file_index=%d size=%d", hash, index, target)
		return target, target, nil
	}
	if err := e.CheckPayloadAdmission(hash); err != nil {
		return 0, target, err
	}

	source, err := e.OpenStreamSource(ctx, hash, index, false)
	if err != nil {
		return 0, target, err
	}
	defer source.Close()

	cleanup, err := e.streamManager.AddPreload(ctx, hash, index, StreamOptions{
		RangeStart:  0,
		HasRange:    true,
		WindowBytes: target,
	})
	if err != nil {
		logging.Debugf("warmup QoS overlay unavailable hash=%s file_index=%d: %v", hash, index, err)
		return 0, target, err
	} else {
		defer cleanup()
	}

	limited := io.LimitReader(source.Reader, target)
	read, err := io.Copy(io.Discard, limited)
	if err != nil && !errors.Is(err, context.Canceled) {
		return read, target, err
	}
	return read, target, nil
}

// BeginShutdown cancels admission and outstanding waits. Close joins owned work before closing resources.
func (e *Engine) BeginShutdown() {
	e.storageTaskMu.Lock()
	if e.cancel != nil {
		e.cancel()
	}
	e.storageTaskMu.Unlock()
}

func (e *Engine) Close() {
	e.closeOnce.Do(func() {
		logging.Infof("closing torrent engine")
		e.BeginShutdown()
		e.admissionMu.Lock()
		e.mu.RLock()
		mts := make([]*ManagedTorrent, 0, len(e.managedTorrents))
		for _, mt := range e.managedTorrents {
			mts = append(mts, mt)
		}
		e.mu.RUnlock()
		for _, mt := range mts {
			if mt == nil || mt.t == nil {
				continue
			}
			mt.mu.Lock()
			d, err := mt.applyStateEventLocked(stateEvent{kind: stateClosed})
			mt.mu.Unlock()
			if err == nil {
				e.applyQuiesceLocked(mt, d, "engine closed")
			}
		}
		e.admissionMu.Unlock()
		if e.streamManager != nil {
			e.streamManager.CloseAllStreams()
		}
		e.wg.Wait()

		e.mu.Lock()
		client := e.client
		torrentStorage := e.storage
		// Client/storage pointers stay immutable for concurrent diagnostic readers.
		// Cancellation and task admission, not a nil pointer, represent shutdown.
		e.mu.Unlock()

		closeTorrentResources(client, torrentStorage)
	})
}

func closeTorrentResources(client *torrent.Client, torrentStorage storage.ClientImplCloser) {
	if client != nil {
		client.Close()
	}
	if torrentStorage != nil {
		if err := torrentStorage.Close(); err != nil {
			logging.Warnf("failed to close torrent storage: %v", err)
		}
	}
}

func (e *Engine) AddMagnet(magnet string) (*models.Torrent, error) {
	return e.addMagnet(magnet, false)
}

// AddStagedMagnet leaves payload/upload gates closed until application commit.
func (e *Engine) AddStagedMagnet(magnet string) (*models.Torrent, error) {
	return e.addMagnet(magnet, true)
}

func (e *Engine) addMagnet(magnet string, staged bool) (*models.Torrent, error) {
	if !e.beginWork() {
		return nil, context.Canceled
	}
	defer e.wg.Done()
	logging.Infof("adding torrent from magnet %s", logging.SafeMagnetSummary(magnet))
	spec, err := torrent.TorrentSpecFromMagnetUri(magnet)
	if err != nil {
		logging.Warnf("failed to parse magnet %s: %v", logging.SafeMagnetSummary(magnet), err)
		return nil, err
	}
	e.augmentTorrentSpec(spec)
	spec.DisallowDataDownload = true
	if staged {
		spec.DisallowDataUpload = true
	}
	t, _, err := e.client.AddTorrentSpec(spec)
	if err != nil {
		logging.Warnf("failed to add magnet spec %s: %v", logging.SafeMagnetSummary(magnet), err)
		return nil, err
	}

	var model *models.Torrent
	if staged {
		model, err = e.registerTorrent(t, magnet, &models.Torrent{State: models.StateQueued})
	} else {
		model, err = e.addTorrentWithSource(t, magnet)
	}
	if err != nil {
		logging.Warnf("failed to register magnet torrent hash=%s: %v", t.InfoHash().HexString(), err)
		return nil, err
	}
	model.SourceURI = magnet
	logging.Infof("torrent added hash=%s source=magnet state=%s", model.Hash, model.State)
	return model, nil
}

func (e *Engine) AddInfoHash(hash string) (*models.Torrent, error) {
	logging.Infof("restoring torrent from bare info hash hash=%s", strings.TrimSpace(hash))
	return e.AddMagnet("magnet:?xt=urn:btih:" + strings.TrimSpace(hash))
}

func (e *Engine) AddTorrentFile(r io.Reader, identity ...string) (*models.Torrent, error) {
	return e.addTorrentFile(r, false, false, identity...)
}

func (e *Engine) AddStagedTorrentFile(r io.Reader, privateSafe ...bool) (*models.Torrent, error) {
	return e.addTorrentFile(r, true, len(privateSafe) > 0 && privateSafe[0])
}

func (e *Engine) addTorrentFile(r io.Reader, staged, privateSafe bool, identity ...string) (*models.Torrent, error) {
	if !e.beginWork() {
		return nil, context.Canceled
	}
	defer e.wg.Done()
	logging.Infof("adding torrent from .torrent file")
	metaInfo, err := LoadMetainfo(r)
	if err != nil {
		logging.Warnf("failed to read .torrent file: %v", err)
		return nil, fmt.Errorf("failed to read torrent file: %w", err)
	}
	logging.Debugf(".torrent file parsed hash=%s trackers=%d", metaInfo.HashInfoBytes().HexString(), len(metaInfo.UpvertedAnnounceList().DistinctValues()))
	spec, err := torrent.TorrentSpecFromMetaInfoErr(metaInfo)
	if err != nil {
		logging.Warnf("failed to build torrent spec hash=%s: %v", metaInfo.HashInfoBytes().HexString(), err)
		return nil, err
	}
	if err := e.saveMetainfo(metaInfo.HashInfoBytes().HexString(), metaInfo, false); err != nil {
		logging.Warnf("failed to persist metainfo hash=%s: %v", metaInfo.HashInfoBytes().HexString(), err)
	}
	spec.PrivateSafe = privateSafe
	e.augmentTorrentSpec(spec)
	spec.DisallowDataDownload = true
	if staged {
		spec.DisallowDataUpload = true
	}
	t, _, err := e.client.AddTorrentSpec(spec)
	if err != nil {
		logging.Warnf("failed to add .torrent hash=%s: %v", metaInfo.HashInfoBytes().HexString(), err)
		return nil, err
	}

	magnet, err := metaInfo.MagnetV2()
	if err != nil {
		return nil, err
	}
	if privateSafe {
		if magnet.Params == nil {
			magnet.Params = make(url.Values)
		}
		magnet.Params.Set("x.hub-private", "1")
	}
	var model *models.Torrent
	if staged {
		model, err = e.registerTorrent(t, magnet.String(), &models.Torrent{State: models.StateQueued}, identity...)
	} else {
		model, err = e.addTorrentWithSource(t, magnet.String(), identity...)
	}
	if err != nil {
		return nil, err
	}
	model.SourceURI = magnet.String()
	if err := e.saveMetainfo(model.Hash, metaInfo, false); err != nil {
		logging.Warnf("failed to persist canonical metainfo hash=%s: %v", model.Hash, err)
	}
	logging.Infof("torrent added hash=%s source=file state=%s", model.Hash, model.State)
	return model, nil
}

func (e *Engine) ScheduleRecheckIfProgressBehind(hash string, persistedDownloaded int64) {
	if persistedDownloaded <= 0 {
		return
	}
	hash = strings.ToLower(strings.TrimSpace(hash))
	e.mu.RLock()
	mt, ok := e.managedTorrents[hash]
	if !ok || mt == nil || mt.t == nil {
		e.mu.RUnlock()
		return
	}
	e.mu.RUnlock()
	mt.mu.Lock()
	mt.pendingRecheckBytes = max(mt.pendingRecheckBytes, persistedDownloaded)
	mt.mu.Unlock()
	e.queueDeferredRecheck(mt)
}

func (e *Engine) addTorrent(t *torrent.Torrent) (*models.Torrent, error) {
	return e.addTorrentWithSource(t, "")
}

func (e *Engine) addTorrentWithSource(t *torrent.Torrent, sourceURI string, identity ...string) (*models.Torrent, error) {
	return e.registerTorrent(t, sourceURI, nil, identity...)
}

func (e *Engine) registerTorrent(t *torrent.Torrent, sourceURI string, restored *models.Torrent, identity ...string) (*models.Torrent, error) {
	if !e.beginWork() {
		return nil, context.Canceled
	}
	defer e.wg.Done()
	e.admissionMu.Lock()
	if e.ctx.Err() != nil {
		e.admissionMu.Unlock()
		t.Drop()
		return nil, e.ctx.Err()
	}
	select {
	case <-t.Closed():
		e.admissionMu.Unlock()
		return nil, fmt.Errorf("torrent generation closed during add")
	default:
	}
	hash := t.InfoHash().HexString()
	if len(identity) > 0 && identity[0] != "" {
		hash = identity[0]
	}
	for key, owner := range e.detaching {
		if key == hash || owner.t == t || owner.t.InfoHash() == t.InfoHash() {
			e.admissionMu.Unlock()
			// An old raw pointer is owned by the detaching task. A newly opened
			// raw pointer cannot become a replacement until that task retires.
			if owner.t != t {
				t.Drop()
			}
			return nil, ErrOperationConflict
		}
	}
	for key, owner := range e.registering {
		if key == hash || owner.t == t || owner.t.InfoHash() == t.InfoHash() {
			e.admissionMu.Unlock()
			if owner.t != t {
				t.Drop()
			}
			return nil, ErrOperationConflict
		}
	}

	e.mu.Lock()
	// A v2-only magnet can acquire its hybrid v1 alias with metadata. Keep the
	// application's original identity for this runtime, never register it twice.
	for key, existing := range e.managedTorrents {
		if existing.t == t {
			hash = key
			break
		}
	}
	if mt, ok := e.managedTorrents[hash]; ok {
		mt.mu.Lock()
		if mt.sourceURI == "" && strings.TrimSpace(sourceURI) != "" {
			mt.sourceURI = strings.TrimSpace(sourceURI)
		}
		mt.mu.Unlock()
		e.mu.Unlock()
		e.admissionMu.Unlock()
		model := e.mapManagedTorrent(mt)
		logging.Debugf("torrent already managed hash=%s state=%s", hash, model.State)
		return model, nil
	}
	e.mu.Unlock()

	mt := &ManagedTorrent{
		hash:                      hash,
		t:                         t,
		sourceURI:                 strings.TrimSpace(sourceURI),
		addedAt:                   time.Now(),
		lastHealthyAt:             time.Now(),
		normalMaxEstablishedConns: e.cfg.BTEstablishedConns,
		filePriorities:            make(map[int]models.FilePriority),
	}
	mt.payloadCtx, mt.payloadCancel = context.WithCancelCause(e.ctx)
	mt.ctx, mt.cancel = context.WithCancel(e.ctx)
	event := stateEvent{kind: stateRegistered, phase: models.StateQueued}
	if restored != nil {
		mt.queuePosition = restored.QueuePosition
		event.phase, event.err, event.diskReason, event.staged = restored.State, restored.Error, restored.DownloadBlock, true
		mt.downloadBlockPersisted = restored.DownloadBlock != ""
		for _, f := range restored.Files {
			if f != nil {
				mt.filePriorities[f.Index] = f.Priority
			}
		}
	}
	if _, err := mt.applyStateEventLocked(event); err != nil {
		e.discardRegistrationLocked(mt)
		return nil, err
	}
	if e.registering == nil {
		e.registering = make(map[string]*ManagedTorrent)
	}
	e.registering[hash] = mt
	persist := e.registerMetainfo
	var persistErr error
	if t.Info() != nil {
		mi := t.Metainfo()
		mt.metainfo = &mi
		e.admissionMu.Unlock()
		if persist != nil {
			persistErr = persist(hash, &mi)
		} else {
			persistErr = e.saveMetainfo(hash, &mi, true)
		}
		e.admissionMu.Lock()
	}
	if persistErr == nil && e.ctx.Err() != nil {
		persistErr = e.ctx.Err()
	}
	select {
	case <-t.Closed():
		if persistErr == nil {
			persistErr = ErrOperationConflict
		}
	default:
	}
	if e.registering[hash] != mt && persistErr == nil {
		persistErr = ErrOperationConflict
	}
	if persistErr != nil {
		e.discardRegistrationLocked(mt)
		return nil, fmt.Errorf("persist runtime metainfo: %w", persistErr)
	}
	delete(e.registering, hash)
	t.SetOnWriteChunkError(func(error) {
		e.queueEvent(mt, "storage-error", func(context.Context) { e.payloadWriteError(mt, nil) })
	})
	e.mu.Lock()
	e.managedTorrents[hash] = mt
	e.statusTorrents.Store(hash, mt)
	logging.Infof("torrent registered hash=%s name=%q state=%s metadata_ready=%t", hash, t.Name(), mt.state, t.Info() != nil)
	e.mu.Unlock()
	e.admissionMu.Unlock()

	e.manageResources()
	if !e.watchMetadata(hash, t) {
		_ = e.Delete(hash)
		return nil, ErrTaskLimit
	}

	return e.mapManagedTorrent(mt), nil
}

// Called with admissionMu; returns with it RELEASED. Failed unpublished raw
// generations reserve their namespace through Drop, just like explicit Delete.
func (e *Engine) discardRegistrationLocked(mt *ManagedTorrent) {
	if e.registering[mt.hash] == mt {
		delete(e.registering, mt.hash)
	}
	if e.detaching == nil {
		e.detaching = make(map[string]*ManagedTorrent)
	}
	e.detaching[mt.hash] = mt
	e.admissionMu.Unlock()
	if mt.cancel != nil {
		mt.cancel()
	}
	mt.t.Drop()
	e.admissionMu.Lock()
	if e.detaching[mt.hash] == mt {
		delete(e.detaching, mt.hash)
	}
	e.admissionMu.Unlock()
}

// GetRawTorrent returns the underlying anacrolix torrent for raw piece data access.
func (e *Engine) GetRawTorrent(hash string) *torrent.Torrent {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if mt, ok := e.managedTorrents[hash]; ok && mt.t != nil {
		return mt.t
	}
	return nil
}

func (e *Engine) Pause(hash string) error {
	return e.pauseWithIntent(hash, nil)
}

// ReconcilePausedIntent installs the complete committed selection/FIFO, not just
// the Pause delta. It never clears independent disk/verification safety keys.
func (e *Engine) ReconcilePausedIntent(saved *models.Torrent) error {
	return e.pauseWithIntent(saved.Hash, saved)
}

func (e *Engine) pauseWithIntent(hash string, saved *models.Torrent) error {
	e.admissionMu.Lock()
	defer e.admissionMu.Unlock()
	if e.ctx.Err() != nil {
		return e.ctx.Err()
	}
	e.mu.RLock()
	mt, ok := e.managedTorrents[hash]
	e.mu.RUnlock()

	if !ok {
		logging.Debugf("pause requested for unmanaged torrent hash=%s", hash)
		return TorrentNotFoundError{Hash: hash}
	}
	var priorities map[int]models.FilePriority
	if saved != nil {
		var err error
		priorities, err = persistedPriorities(saved, mt.t)
		if err != nil {
			return err
		}
	}

	mt.t.DisallowDataDownload()
	mt.mu.Lock()
	d, err := mt.applyStateEventLocked(stateEvent{kind: statePaused, fullReconcile: saved != nil})
	if err != nil {
		mt.mu.Unlock()
		return err
	}
	if saved != nil {
		mt.filePriorities = priorities
		mt.queuePosition = saved.QueuePosition
	}
	mt.mu.Unlock()
	e.applyQuiesceLocked(mt, d, "torrent is paused")

	return nil
}

func (e *Engine) Resume(hash string) error {
	e.admissionMu.Lock()
	err := e.resumeLocked(hash)
	e.admissionMu.Unlock()
	if err == nil {
		e.manageResources()
		if current := e.GetTorrent(hash); current != nil && current.DownloadBlock != "" {
			return DownloadBlockedError{Reason: current.DownloadBlock}
		}
	}
	return err
}

func (e *Engine) resumeLocked(hash string) error {
	if e.ctx.Err() != nil {
		return e.ctx.Err()
	}
	e.mu.RLock()
	mt, ok := e.managedTorrents[hash]
	e.mu.RUnlock()

	if !ok {
		logging.Debugf("resume requested for unmanaged torrent hash=%s", hash)
		return TorrentNotFoundError{Hash: hash}
	}

	mt.mu.Lock()
	if mt.rechecking || mt.verificationUncertain {
		mt.mu.Unlock()
		return ErrOperationConflict
	}
	if mt.initializing {
		mt.mu.Unlock()
		return fmt.Errorf("torrent initialization is not complete")
	}
	mt.mu.Unlock()
	mt.t.DisallowDataDownload()
	writeVersion := mt.t.WriteChunkErrorVersion()
	probe, minimum := e.diskProbe, e.minimumDiskBytes()
	var probeErr error
	if err := e.runRuntimeWorkLocked(mt, func() { probeErr = e.diskErrorWithProbe(hash, probe, minimum) }); err != nil {
		return err
	}
	if err := probeErr; err != nil {
		if persistErr := e.blockDownloadLocked(mt, err.Error()); persistErr != nil {
			return persistErr
		}
		return DownloadBlockedError{err.Error()}
	}
	if err := mt.t.ClearWriteChunkErrorIfVersion(writeVersion); err != nil {
		if persistErr := e.blockDownloadLocked(mt, "payload write failed during Resume"); persistErr != nil {
			return persistErr
		}
		return DownloadBlockedError{Reason: "payload write failed during Resume"}
	}
	if e.saveDownloadBlock != nil {
		if err := e.saveDownloadBlock(hash, ""); err != nil {
			return err
		}
	}
	mt.mu.Lock()
	_, err := mt.applyStateEventLocked(stateEvent{kind: stateActivated, phase: models.StateQueued, diskRecovered: true})
	if err != nil {
		mt.mu.Unlock()
		return err
	}
	mt.downloadBlockPersisted = false
	mt.payloadCtx, mt.payloadCancel = context.WithCancelCause(e.ctx)
	mt.metadataWaitLogged = false
	mt.mu.Unlock()
	// Install base priorities before permitting requests, including reader-driven
	// requests. Metadata-pending torrents are admitted by watchMetadata instead.
	if mt.t.Info() != nil {
		if err := e.applyFilePrioritiesAndDownload(mt); err != nil {
			return err
		}
		mt.t.AllowDataUpload()
	}
	return nil
}

func (e *Engine) Delete(hash string) error {
	if !e.beginWork() {
		return context.Canceled
	}
	defer e.wg.Done()
	e.admissionMu.Lock()
	if e.ctx.Err() != nil {
		e.admissionMu.Unlock()
		return e.ctx.Err()
	}
	if e.detaching[hash] != nil {
		e.admissionMu.Unlock()
		return ErrOperationConflict
	}
	for key, owner := range e.registering {
		if key == hash || owner.t.InfoHash().HexString() == hash {
			e.admissionMu.Unlock()
			return ErrOperationConflict
		}
	}
	e.mu.Lock()
	mt, ok := e.managedTorrents[hash]
	if !ok {
		e.mu.Unlock()
		e.admissionMu.Unlock()
		logging.Debugf("delete requested for unmanaged torrent hash=%s", hash)
		return nil
	}

	delete(e.managedTorrents, hash)
	e.statusTorrents.Range(func(key, value any) bool {
		if value == mt {
			e.statusTorrents.Delete(key)
		}
		return true
	})
	e.mu.Unlock()
	if e.detaching == nil {
		e.detaching = make(map[string]*ManagedTorrent)
	}
	e.detaching[hash] = mt
	if mt.cancel != nil {
		mt.cancel()
	}
	mt.mu.Lock()
	d, transitionErr := mt.applyStateEventLocked(stateEvent{kind: stateDeleted})
	mt.mu.Unlock()
	if transitionErr == nil {
		e.applyQuiesceLocked(mt, d, "torrent deleted")
	}

	logging.Infof("deleting torrent hash=%s", hash)
	var handles []*directStreamHandle
	if e.streamManager != nil {
		handles = e.streamManager.detachStreamsForTorrentLocked(mt)
	}
	drop := e.dropTorrent
	e.admissionMu.Unlock()
	closeDirectHandles(handles)
	if drop != nil {
		drop(mt.t)
	} else {
		mt.t.Drop()
	}
	e.admissionMu.Lock()
	if e.detaching[hash] == mt {
		delete(e.detaching, hash)
	}
	e.admissionMu.Unlock()
	return nil
}

func (e *Engine) GetAllTorrents() []*models.Torrent { return e.allTorrents(true) }

// GetAllTorrentsCached samples counters without filesystem work. It never grants read admission.
func (e *Engine) GetAllTorrentsCached() []*models.Torrent { return e.allTorrents(false) }

func (e *Engine) allTorrents(refresh bool) []*models.Torrent {
	e.mu.RLock()
	mts := make([]*ManagedTorrent, 0, len(e.managedTorrents))
	for _, mt := range e.managedTorrents {
		mts = append(mts, mt)
	}
	e.mu.RUnlock()

	torrents := make([]*models.Torrent, 0, len(mts))
	for _, mt := range mts {
		if refresh {
			mt.t.RefreshStorageCompletion()
		}
		torrents = append(torrents, e.mapManagedTorrent(mt))
	}
	return torrents
}

func (e *Engine) MetainfoSettings() (string, string) {
	return e.cfg.MetainfoTempDir, e.cfg.MetainfoAllowlist
}
