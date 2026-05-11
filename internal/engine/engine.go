package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"

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
	client  *torrent.Client
	storage storage.ClientImplCloser
	cfg     *config.Config

	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once

	mu                        sync.RWMutex
	managedTorrents           map[string]*ManagedTorrent
	lastResourceLog           time.Time
	lastResourceKey           string
	invalidMetainfoCount      int
	pieceCompletionBackend    string
	pieceCompletionPersistent bool
	pieceCompletionErr        string

	streamManager *StreamManager
}

type ManagedTorrent struct {
	mu                        sync.Mutex
	t                         *torrent.Torrent
	state                     models.TorrentState
	err                       models.ErrorReason
	metadataLogged            bool
	metadataWaitLogged        bool
	downloadAllStarted        bool
	lastStatsAt               time.Time
	lastRawReadBytes          int64
	lastDataReadBytes         int64
	lastReadBytes             int64
	lastWrittenBytes          int64
	rawDownloadSpeed          int64
	dataDownloadSpeed         int64
	downloadSpeed             int64
	uploadSpeed               int64
	lastPeerSummary           models.PeerSummary
	lastPeerLog               time.Time
	trackerStatus             string
	trackerError              string
	sourceURI                 string
	metainfo                  *metainfo.MetaInfo
	addedAt                   time.Time
	degraded                  bool
	lastSwarmCheckAt          time.Time
	lastSwarmRefreshAt        time.Time
	lastSwarmRefreshReason    string
	lastPeerRefreshAt         time.Time
	lastPeerRefreshReason     string
	lastHealthyAt             time.Time
	stallStartedAt            time.Time
	peakDownloadSpeed         int64
	peakUpdatedAt             time.Time
	boostUntil                time.Time
	normalMaxEstablishedConns int
	filePriorities            map[int]models.FilePriority
}

func New(cfg *config.Config) (*Engine, error) {
	config.ApplyDefaults(cfg)
	logging.Infof("initializing torrent engine download_dir=%s torrent_port=%d max_downloads=%d min_free_space_gb=%d bt_seed=%t bt_no_upload=%t bt_profile=%s bt_download_profile=%s bt_benchmark=%t bt_retrackers=%s dht=%t pex=%t upnp=%t tcp=%t utp=%t ipv6=%t established_conns=%d half_open=%d total_half_open=%d peers_low=%d peers_high=%d dial_rate=%d swarm_watchdog=%t swarm_check_sec=%d swarm_refresh_cooldown_sec=%d swarm_min_peers=%d swarm_min_seeds=%d swarm_stalled_speed=%d swarm_stalled_duration_sec=%d swarm_boost_conns=%d swarm_boost_duration_sec=%d peer_drop_ratio=%.2f seed_drop_ratio=%.2f speed_drop_ratio=%.2f peak_ttl_sec=%d hard_refresh=%t auto_hard_refresh=%t hard_refresh_cooldown_sec=%d hard_refresh_after_soft_fails=%d hard_refresh_min_age_sec=%d episode_ttl_sec=%d recovery_grace_sec=%d client_recycle=%t client_recycle_cooldown_sec=%d client_recycle_after_soft_fails=%d client_recycle_min_age_sec=%d client_recycle_after_hard_fails=%d client_recycle_max_per_hour=%d",
		cfg.DownloadDir,
		cfg.TorrentPort,
		cfg.MaxActiveDownloads,
		cfg.MinFreeSpaceGB,
		cfg.BTSeed,
		cfg.BTNoUpload,
		cfg.BTClientProfile,
		cfg.BTDownloadProfile,
		cfg.BTBenchmarkMode,
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
		cfg.BTSwarmHardRefreshEnabled,
		cfg.BTSwarmAutoHardRefreshEnabled,
		cfg.BTSwarmHardRefreshCooldownSec,
		cfg.BTSwarmHardRefreshAfterSoftFails,
		cfg.BTSwarmHardRefreshMinTorrentAgeSec,
		cfg.BTSwarmDegradationEpisodeTTLSec,
		cfg.BTSwarmRecoveryGraceSec,
		cfg.BTClientRecycleEnabled,
		cfg.BTClientRecycleCooldownSec,
		cfg.BTClientRecycleAfterSoftFails,
		cfg.BTClientRecycleMinTorrentAgeSec,
		cfg.BTClientRecycleAfterHardFails,
		cfg.BTClientRecycleMaxPerHour,
	)

	ctx, cancel := context.WithCancel(context.Background())
	eng := &Engine{
		cfg:             cfg,
		ctx:             ctx,
		cancel:          cancel,
		managedTorrents: make(map[string]*ManagedTorrent),
	}
	client, torrentStorage, err := eng.newTorrentClient()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create torrent client: %w", err)
	}
	eng.client = client
	eng.storage = torrentStorage
	eng.streamManager = NewStreamManager(eng)

	eng.startBackground(eng.resourceMonitor)
	if cfg.BTSwarmWatchdogEnabled {
		eng.startBackground(eng.swarmRefreshMonitor)
	}

	return eng, nil
}

func (e *Engine) startBackground(fn func(context.Context)) {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		fn(e.ctx)
	}()
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
	return e.mapManagedTorrent(mt)
}

func (e *Engine) Warmup(ctx context.Context, hash string, index int, size int64) (int64, int64, error) {
	file, err := e.GetTorrentFile(hash, index)
	if err != nil {
		return 0, 0, err
	}
	file.SetPriority(torrent.PiecePriorityHigh)

	reader := file.NewReader()
	reader.SetContext(ctx)
	reader.SetResponsive()
	defer reader.Close()

	if size <= 0 {
		size = 20 << 20
	}
	if size > file.Length() {
		size = file.Length()
	}
	limited := io.LimitReader(reader, size)
	read, err := io.Copy(io.Discard, limited)
	if err != nil && !errors.Is(err, context.Canceled) {
		return read, size, err
	}
	return read, size, nil
}

func (e *Engine) Close() {
	e.closeOnce.Do(func() {
		logging.Infof("closing torrent engine")
		if e.cancel != nil {
			e.cancel()
		}
		e.wg.Wait()

		e.mu.Lock()
		client := e.client
		torrentStorage := e.storage
		e.client = nil
		e.storage = nil
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
	logging.Infof("adding torrent from magnet %s", logging.SafeMagnetSummary(magnet))
	spec, err := torrent.TorrentSpecFromMagnetUri(magnet)
	if err != nil {
		logging.Warnf("failed to parse magnet %s: %v", logging.SafeMagnetSummary(magnet), err)
		return nil, err
	}
	e.augmentTorrentSpec(spec)
	t, _, err := e.client.AddTorrentSpec(spec)
	if err != nil {
		logging.Warnf("failed to add magnet spec %s: %v", logging.SafeMagnetSummary(magnet), err)
		return nil, err
	}

	model, err := e.addTorrentWithSource(t, magnet)
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

func (e *Engine) AddTorrentFile(r io.Reader) (*models.Torrent, error) {
	logging.Infof("adding torrent from .torrent file")
	metaInfo, err := metainfo.Load(r)
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
	e.augmentTorrentSpec(spec)
	t, _, err := e.client.AddTorrentSpec(spec)
	if err != nil {
		logging.Warnf("failed to add .torrent hash=%s: %v", metaInfo.HashInfoBytes().HexString(), err)
		return nil, err
	}

	model, err := e.addTorrentWithSource(t, metaInfo.Magnet(nil, nil).String())
	if err != nil {
		return nil, err
	}
	model.SourceURI = metaInfo.Magnet(nil, nil).String()
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
	t := mt.t
	e.mu.RUnlock()

	go func() {
		<-t.GotInfo()

		e.mu.Lock()
		mt, ok := e.managedTorrents[hash]
		if !ok || mt.t != t {
			e.mu.Unlock()
			return
		}
		if t.BytesCompleted() >= persistedDownloaded {
			e.mu.Unlock()
			return
		}
		if t.Info() != nil {
			for _, f := range t.Files() {
				f.SetPriority(torrent.PiecePriorityNone)
			}
			mt.downloadAllStarted = false
		}
		e.mu.Unlock()

		logging.Infof("rechecking existing torrent data hash=%s runtime_downloaded=%d persisted_downloaded=%d", hash, t.BytesCompleted(), persistedDownloaded)
		if err := t.VerifyDataContext(context.Background()); err != nil {
			logging.Warnf("torrent data recheck failed hash=%s: %v", hash, err)
		} else {
			logging.Infof("torrent data recheck completed hash=%s downloaded=%d", hash, t.BytesCompleted())
		}

		e.mu.Lock()
		defer e.mu.Unlock()
		mt, ok = e.managedTorrents[hash]
		if !ok || mt.t != t {
			return
		}
		if mt.state == models.StateDownloading && t.Info() != nil {
			e.applyFilePrioritiesAndDownload(mt)
			mt.downloadAllStarted = true
			logging.Debugf("download all restarted after data recheck hash=%s files=%d", hash, len(t.Files()))
		}
	}()
}

func (e *Engine) addTorrent(t *torrent.Torrent) (*models.Torrent, error) {
	return e.addTorrentWithSource(t, "")
}

func (e *Engine) addTorrentWithSource(t *torrent.Torrent, sourceURI string) (*models.Torrent, error) {
	hash := t.InfoHash().HexString()

	e.mu.Lock()
	if mt, ok := e.managedTorrents[hash]; ok {
		if mt.sourceURI == "" && strings.TrimSpace(sourceURI) != "" {
			mt.sourceURI = strings.TrimSpace(sourceURI)
		}
		e.mu.Unlock()
		model := e.mapManagedTorrent(mt)
		logging.Debugf("torrent already managed hash=%s state=%s", hash, model.State)
		return model, nil
	}

	mt := &ManagedTorrent{
		t:                         t,
		state:                     models.StateQueued, // initially queued, resourceMonitor will start it
		err:                       models.ErrNone,
		sourceURI:                 strings.TrimSpace(sourceURI),
		addedAt:                   time.Now(),
		lastHealthyAt:             time.Now(),
		normalMaxEstablishedConns: e.cfg.BTEstablishedConns,
		filePriorities:            make(map[int]models.FilePriority),
	}
	if t.Info() != nil {
		mi := t.Metainfo()
		mt.metainfo = &mi
		if err := e.saveMetainfo(hash, &mi, true); err != nil {
			logging.Warnf("failed to persist runtime metainfo hash=%s: %v", hash, err)
		}
	}
	e.managedTorrents[hash] = mt
	logging.Infof("torrent registered hash=%s name=%q state=%s metadata_ready=%t", hash, t.Name(), mt.state, t.Info() != nil)
	e.mu.Unlock()

	model := e.mapManagedTorrent(mt)
	e.manageResources()
	e.watchMetadata(hash, t)

	return model, nil
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
	e.mu.RLock()
	mt, ok := e.managedTorrents[hash]
	e.mu.RUnlock()

	if !ok {
		logging.Debugf("pause requested for unmanaged torrent hash=%s", hash)
		return TorrentNotFoundError{Hash: hash}
	}

	if mt.t.Info() != nil {
		for _, f := range mt.t.Files() {
			f.SetPriority(torrent.PiecePriorityNone)
		}
		mt.t.CancelPieces(0, mt.t.NumPieces())
		mt.t.DisallowDataUpload()
		mt.t.DisallowDataDownload()
	} else {
		logging.Debugf("pause requested before metadata is ready hash=%s", hash)
	}

	mt.mu.Lock()
	mt.downloadAllStarted = false
	mt.setStateLocked(models.StatePaused, models.ErrNone, "pause requested")
	mt.mu.Unlock()

	return nil
}

func (e *Engine) Resume(hash string) error {
	e.mu.RLock()
	mt, ok := e.managedTorrents[hash]
	e.mu.RUnlock()

	if !ok {
		logging.Debugf("resume requested for unmanaged torrent hash=%s", hash)
		return TorrentNotFoundError{Hash: hash}
	}

	if mt.t != nil {
		mt.t.AllowDataUpload()
		mt.t.AllowDataDownload()
	}

	mt.mu.Lock()
	mt.downloadAllStarted = false
	mt.metadataWaitLogged = false
	mt.setStateLocked(models.StateQueued, models.ErrNone, "resume requested") // put to queue, let resource monitor handle it
	mt.mu.Unlock()

	e.manageResources()
	return nil
}

func (e *Engine) Delete(hash string) error {
	e.mu.Lock()
	mt, ok := e.managedTorrents[hash]
	if !ok {
		e.mu.Unlock()
		logging.Debugf("delete requested for unmanaged torrent hash=%s", hash)
		return nil
	}

	delete(e.managedTorrents, hash)
	e.mu.Unlock()

	logging.Infof("deleting torrent hash=%s", hash)
	mt.t.Drop()
	return nil
}

func (e *Engine) GetAllTorrents() []*models.Torrent {
	e.mu.RLock()
	mts := make([]*ManagedTorrent, 0, len(e.managedTorrents))
	for _, mt := range e.managedTorrents {
		mts = append(mts, mt)
	}
	e.mu.RUnlock()

	torrents := make([]*models.Torrent, 0, len(mts))
	for _, mt := range mts {
		torrents = append(torrents, e.mapManagedTorrent(mt))
	}
	return torrents
}

func (mt *ManagedTorrent) setStateLocked(state models.TorrentState, errReason models.ErrorReason, reason string) {
	oldState := mt.state
	oldErr := mt.err
	mt.state = state
	mt.err = errReason
	if oldState != state || oldErr != errReason {
		logging.Infof("torrent state change hash=%s state=%s->%s error=%s reason=%s", mt.t.InfoHash().HexString(), oldState, state, errReason, reason)
	}
}
