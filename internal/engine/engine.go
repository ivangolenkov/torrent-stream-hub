package engine

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
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
	"golang.org/x/time/rate"
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
	client *torrent.Client
	cfg    *config.Config

	mu                      sync.RWMutex
	managedTorrents         map[string]*ManagedTorrent
	lastResourceLog         time.Time
	lastResourceKey         string
	recyclingClient         bool
	clientRecycleCount      int
	clientRecycleTimes      []time.Time
	lastClientRecycleAt     time.Time
	lastClientRecycleReason string
	lastClientRecycleErr    string
	recycleScheduledReason  string

	streamManager *StreamManager
}

type ManagedTorrent struct {
	t                               *torrent.Torrent
	state                           models.TorrentState
	err                             models.ErrorReason
	metadataLogged                  bool
	metadataWaitLogged              bool
	downloadAllStarted              bool
	lastStatsAt                     time.Time
	lastReadBytes                   int64
	lastWrittenBytes                int64
	downloadSpeed                   int64
	uploadSpeed                     int64
	lastPeerSummary                 models.PeerSummary
	lastPeerLog                     time.Time
	trackerStatus                   string
	trackerError                    string
	sourceURI                       string
	metainfo                        *metainfo.MetaInfo
	lastReaddSource                 string
	addedAt                         time.Time
	peakConnected                   int
	peakSeeds                       int
	peakDownloadSpeed               int64
	peakUpdatedAt                   time.Time
	softRefreshCount                int
	hardRefreshCount                int
	lastHardRefreshAt               time.Time
	lastHardRefreshReason           string
	lastHardRefreshErr              string
	pendingHardRefresh              bool
	degradationEpisodeStartedAt     time.Time
	lastDegradedAt                  time.Time
	lastRecoveredAt                 time.Time
	lastSoftRefreshAt               time.Time
	lastSoftRefreshReason           string
	softRefreshAttemptsInEpisode    int
	hardRefreshAttemptsInEpisode    int
	lastSoftRefreshCountResetReason string
	nextHardRefreshAt               time.Time
	nextClientRecycleAt             time.Time
	degraded                        bool
	lastSwarmCheckAt                time.Time
	lastSwarmRefreshAt              time.Time
	lastSwarmRefreshReason          string
	lastHealthyAt                   time.Time
	stallStartedAt                  time.Time
	boostUntil                      time.Time
	normalMaxEstablishedConns       int
}

func New(cfg *config.Config) (*Engine, error) {
	config.ApplyDefaults(cfg)
	logging.Infof("initializing torrent engine download_dir=%s torrent_port=%d max_downloads=%d min_free_space_gb=%d bt_seed=%t bt_no_upload=%t bt_profile=%s bt_retrackers=%s dht=%t pex=%t upnp=%t tcp=%t utp=%t ipv6=%t established_conns=%d half_open=%d total_half_open=%d peers_low=%d peers_high=%d dial_rate=%d swarm_watchdog=%t swarm_check_sec=%d swarm_refresh_cooldown_sec=%d swarm_min_peers=%d swarm_min_seeds=%d swarm_stalled_speed=%d swarm_stalled_duration_sec=%d swarm_boost_conns=%d swarm_boost_duration_sec=%d peer_drop_ratio=%.2f seed_drop_ratio=%.2f speed_drop_ratio=%.2f peak_ttl_sec=%d hard_refresh=%t auto_hard_refresh=%t hard_refresh_cooldown_sec=%d hard_refresh_after_soft_fails=%d hard_refresh_min_age_sec=%d episode_ttl_sec=%d recovery_grace_sec=%d client_recycle=%t client_recycle_cooldown_sec=%d client_recycle_after_soft_fails=%d client_recycle_min_age_sec=%d client_recycle_after_hard_fails=%d client_recycle_max_per_hour=%d",
		cfg.DownloadDir,
		cfg.TorrentPort,
		cfg.MaxActiveDownloads,
		cfg.MinFreeSpaceGB,
		cfg.BTSeed,
		cfg.BTNoUpload,
		cfg.BTClientProfile,
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

	eng := &Engine{
		cfg:             cfg,
		managedTorrents: make(map[string]*ManagedTorrent),
	}
	client, err := eng.newTorrentClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create torrent client: %w", err)
	}
	eng.client = client
	eng.streamManager = NewStreamManager(eng)

	go eng.resourceMonitor()
	if cfg.BTSwarmWatchdogEnabled {
		go eng.swarmRefreshMonitor()
	}

	return eng, nil
}

func (e *Engine) newTorrentClient() (*torrent.Client, error) {
	clientConfig := buildClientConfig(e.cfg)
	clientConfig.Callbacks.StatusUpdated = append(clientConfig.Callbacks.StatusUpdated, func(event torrent.StatusUpdatedEvent) {
		e.handleStatusEvent(event)
	})
	clientConfig.Callbacks.PeerConnAdded = append(clientConfig.Callbacks.PeerConnAdded, func(conn *torrent.PeerConn) {
		e.logPeerConnEvent("added", conn)
	})
	clientConfig.Callbacks.PeerConnClosed = func(conn *torrent.PeerConn) {
		e.logPeerConnEvent("closed", conn)
	}
	return torrent.NewClient(clientConfig)
}

func buildClientConfig(cfg *config.Config) *torrent.ClientConfig {
	config.ApplyDefaults(cfg)
	clientConfig := torrent.NewDefaultClientConfig()
	clientConfig.DataDir = cfg.DownloadDir
	clientConfig.ListenPort = cfg.TorrentPort
	clientConfig.DefaultStorage = storage.NewFile(cfg.DownloadDir)
	clientConfig.Seed = cfg.BTSeed
	clientConfig.NoUpload = cfg.BTNoUpload
	clientConfig.NoDHT = cfg.BTDisableDHT
	clientConfig.DisablePEX = cfg.BTDisablePEX
	clientConfig.NoDefaultPortForwarding = cfg.BTDisableUPNP
	clientConfig.DisableTCP = cfg.BTDisableTCP
	clientConfig.DisableUTP = cfg.BTDisableUTP
	clientConfig.DisableIPv6 = cfg.BTDisableIPv6
	clientConfig.EstablishedConnsPerTorrent = cfg.BTEstablishedConns
	clientConfig.HalfOpenConnsPerTorrent = cfg.BTHalfOpenConns
	clientConfig.TotalHalfOpenConns = cfg.BTTotalHalfOpen
	clientConfig.TorrentPeersLowWater = cfg.BTPeersLowWater
	clientConfig.TorrentPeersHighWater = cfg.BTPeersHighWater
	clientConfig.DialRateLimiter = rate.NewLimiter(rate.Limit(cfg.BTDialRateLimit), cfg.BTDialRateLimit)

	if strings.EqualFold(cfg.BTClientProfile, "qbittorrent") || cfg.BTClientProfile == "" {
		applyQBittorrentProfile(clientConfig)
	} else if !strings.EqualFold(cfg.BTClientProfile, "default") {
		logging.Warnf("unknown bt client profile %q, using qbittorrent", cfg.BTClientProfile)
		applyQBittorrentProfile(clientConfig)
	}

	if cfg.DownloadLimit > 0 {
		clientConfig.DownloadRateLimiter = rate.NewLimiter(rate.Limit(cfg.DownloadLimit), cfg.DownloadLimit)
	}
	if cfg.UploadLimit > 0 {
		clientConfig.UploadRateLimiter = rate.NewLimiter(rate.Limit(cfg.UploadLimit), cfg.UploadLimit)
	}

	return clientConfig
}

func applyQBittorrentProfile(clientConfig *torrent.ClientConfig) {
	const (
		userAgent = "qBittorrent/4.3.9"
		peerID    = "-qB4390-"
	)
	clientConfig.HTTPUserAgent = userAgent
	clientConfig.ExtendedHandshakeClientVersion = userAgent
	clientConfig.Bep20 = peerID
	clientConfig.PeerID = randomPeerID(peerID)
}

func randomPeerID(prefix string) string {
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		logging.Warnf("failed to generate random peer id, using deterministic fallback: %v", err)
		return (prefix + "00000000000000000000")[:20]
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(randomBytes)
	return (prefix + encoded)[:20]
}

func (e *Engine) StreamManager() *StreamManager {
	return e.streamManager
}

func (e *Engine) DownloadDir() string {
	return e.cfg.DownloadDir
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
	e.mu.Lock()
	defer e.mu.Unlock()

	mt, ok := e.managedTorrents[hash]
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
	logging.Infof("closing torrent engine")
	e.client.Close()
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

func (e *Engine) augmentTorrentSpec(spec *torrent.TorrentSpec) {
	if spec == nil {
		return
	}
	trackers := e.retrackers()
	if len(trackers) == 0 {
		return
	}

	switch strings.ToLower(strings.TrimSpace(e.cfg.BTRetrackersMode)) {
	case "off":
		return
	case "replace":
		spec.Trackers = [][]string{trackers}
	case "", "append":
		spec.Trackers = appendTrackerTier(spec.Trackers, trackers)
	default:
		logging.Warnf("unknown retrackers mode %q, using append", e.cfg.BTRetrackersMode)
		spec.Trackers = appendTrackerTier(spec.Trackers, trackers)
	}
}

func (e *Engine) retrackers() []string {
	return mergeTrackers(defaultRetrackers(), loadTrackersFile(e.cfg.BTRetrackersFile))
}

func appendTrackerTier(existing [][]string, trackers []string) [][]string {
	if len(trackers) == 0 {
		return existing
	}
	merged := make([][]string, 0, len(existing)+1)
	for _, tier := range existing {
		cleanTier := mergeTrackers(tier)
		if len(cleanTier) > 0 {
			merged = append(merged, cleanTier)
		}
	}
	merged = append(merged, mergeTrackers(trackers))
	return merged
}

func mergeTrackers(groups ...[]string) []string {
	seen := make(map[string]bool)
	var merged []string
	for _, group := range groups {
		for _, tracker := range group {
			tracker = strings.TrimSpace(tracker)
			if tracker == "" || seen[tracker] || !validTrackerURL(tracker) {
				continue
			}
			seen[tracker] = true
			merged = append(merged, tracker)
		}
	}
	return merged
}

func validTrackerURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "udp", "http", "https", "ws", "wss":
		return true
	default:
		return false
	}
}

func loadTrackersFile(path string) []string {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logging.Warnf("failed to read retrackers file path=%s: %v", path, err)
		}
		return nil
	}
	return strings.Split(string(buf), "\n")
}

func defaultRetrackers() []string {
	return []string{
		"http://retracker.local/announce",
		"http://bt4.t-ru.org/ann?magnet",
		"http://retracker.mgts.by:80/announce",
		"http://tracker.city9x.com:2710/announce",
		"http://tracker.electro-torrent.pl:80/announce",
		"http://tracker.internetwarriors.net:1337/announce",
		"http://tracker2.itzmx.com:6961/announce",
		"udp://opentor.org:2710",
		"udp://public.popcorn-tracker.org:6969/announce",
		"udp://tracker.opentrackr.org:1337/announce",
		"http://bt.svao-ix.ru/announce",
		"udp://explodie.org:6969",
		"wss://tracker.btorrent.xyz",
		"wss://tracker.openwebtorrent.com",
	}
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
		model := e.mapManagedTorrent(mt)
		e.mu.Unlock()
		logging.Debugf("torrent already managed hash=%s state=%s", hash, model.State)
		return model, nil
	}

	mt := &ManagedTorrent{
		t:                               t,
		state:                           models.StateQueued, // initially queued, resourceMonitor will start it
		err:                             models.ErrNone,
		sourceURI:                       strings.TrimSpace(sourceURI),
		addedAt:                         time.Now(),
		lastHealthyAt:                   time.Now(),
		lastSoftRefreshCountResetReason: "new torrent",
		normalMaxEstablishedConns:       e.cfg.BTEstablishedConns,
	}
	if t.Info() != nil {
		mi := t.Metainfo()
		mt.metainfo = &mi
	}
	e.managedTorrents[hash] = mt
	logging.Infof("torrent registered hash=%s name=%q state=%s metadata_ready=%t", hash, t.Name(), mt.state, t.Info() != nil)
	e.manageResourcesLocked()
	model := e.mapManagedTorrent(mt)
	e.mu.Unlock()

	e.watchMetadata(hash, t)

	return model, nil
}

func (e *Engine) Pause(hash string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	mt, ok := e.managedTorrents[hash]
	if !ok {
		logging.Debugf("pause requested for unmanaged torrent hash=%s", hash)
		return TorrentNotFoundError{Hash: hash}
	}

	if mt.t.Info() != nil {
		for _, f := range mt.t.Files() {
			f.SetPriority(torrent.PiecePriorityNone)
		}
	} else {
		logging.Debugf("pause requested before metadata is ready hash=%s", hash)
	}
	mt.downloadAllStarted = false
	e.setStateLocked(hash, mt, models.StatePaused, models.ErrNone, "pause requested")
	return nil
}

func (e *Engine) Resume(hash string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	mt, ok := e.managedTorrents[hash]
	if !ok {
		logging.Debugf("resume requested for unmanaged torrent hash=%s", hash)
		return TorrentNotFoundError{Hash: hash}
	}

	mt.downloadAllStarted = false
	mt.metadataWaitLogged = false
	e.setStateLocked(hash, mt, models.StateQueued, models.ErrNone, "resume requested") // put to queue, let resource monitor handle it
	e.manageResourcesLocked()
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
	e.mu.Lock()
	defer e.mu.Unlock()

	torrents := make([]*models.Torrent, 0, len(e.managedTorrents))
	for _, mt := range e.managedTorrents {
		torrents = append(torrents, e.mapManagedTorrent(mt))
	}
	return torrents
}

func (e *Engine) BTHealth() *models.BTHealth {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	recycleBlocked := e.clientRecycleBlockedReasonLocked(now)

	health := &models.BTHealth{
		SeedEnabled:                   e.cfg.BTSeed,
		UploadEnabled:                 !e.cfg.BTNoUpload,
		DHTEnabled:                    !e.cfg.BTDisableDHT,
		PEXEnabled:                    !e.cfg.BTDisablePEX,
		UPNPEnabled:                   !e.cfg.BTDisableUPNP,
		TCPEnabled:                    !e.cfg.BTDisableTCP,
		UTPEnabled:                    !e.cfg.BTDisableUTP,
		IPv6Enabled:                   !e.cfg.BTDisableIPv6,
		ListenPort:                    e.cfg.TorrentPort,
		ClientProfile:                 normalizedBTClientProfile(e.cfg.BTClientProfile),
		RetrackersMode:                normalizedRetrackersMode(e.cfg.BTRetrackersMode),
		DownloadLimit:                 e.cfg.DownloadLimit,
		UploadLimit:                   e.cfg.UploadLimit,
		SwarmWatchdogEnabled:          e.cfg.BTSwarmWatchdogEnabled,
		SwarmCheckIntervalSec:         e.cfg.BTSwarmCheckIntervalSec,
		SwarmRefreshCooldownSec:       e.cfg.BTSwarmRefreshCooldownSec,
		HardRefreshEnabled:            e.cfg.BTSwarmHardRefreshEnabled,
		AutoHardRefreshEnabled:        e.cfg.BTSwarmAutoHardRefreshEnabled,
		HardRefreshCooldownSec:        e.cfg.BTSwarmHardRefreshCooldownSec,
		HardRefreshAfterSoftFails:     e.cfg.BTSwarmHardRefreshAfterSoftFails,
		ClientRecycleEnabled:          e.cfg.BTClientRecycleEnabled,
		ClientRecycleCooldownSec:      e.cfg.BTClientRecycleCooldownSec,
		ClientRecycleAfterHardFails:   e.cfg.BTClientRecycleAfterHardFails,
		ClientRecycleAfterSoftFails:   e.cfg.BTClientRecycleAfterSoftFails,
		ClientRecycleMinTorrentAgeSec: e.cfg.BTClientRecycleMinTorrentAgeSec,
		ClientRecycleCount:            e.clientRecycleCount,
		ClientRecycleCountLastHour:    e.clientRecycleCountLastHourLocked(now),
		LastClientRecycleAt:           formatBTHealthTime(e.lastClientRecycleAt),
		LastClientRecycleReason:       e.lastClientRecycleReason,
		LastClientRecycleError:        e.lastClientRecycleErr,
		ClientRecycleAllowed:          recycleBlocked == "",
		ClientRecycleBlockedReason:    recycleBlocked,
		NextClientRecycleAt:           formatBTHealthTime(nextClientRecycleAt(e.cfg, e.lastClientRecycleAt, now)),
		RecycleScheduledReason:        e.recycleScheduledReason,
		PeerDropRatio:                 e.cfg.BTSwarmPeerDropRatio,
		SeedDropRatio:                 e.cfg.BTSwarmSeedDropRatio,
		SpeedDropRatio:                e.cfg.BTSwarmSpeedDropRatio,
		IncomingConnectivityNote:      "Incoming peers may not reach this client unless TCP/UDP torrent port is forwarded or UPnP succeeds.",
		Torrents:                      make([]models.BTTorrentHealth, 0, len(e.managedTorrents)),
	}

	for _, mt := range e.managedTorrents {
		stats := mt.t.Stats()
		e.updateSpeedsLocked(mt, stats)
		maxEstablishedConns := mt.normalMaxEstablishedConns
		if maxEstablishedConns == 0 {
			maxEstablishedConns = e.cfg.BTEstablishedConns
		}
		if !mt.boostUntil.IsZero() && now.Before(mt.boostUntil) {
			maxEstablishedConns = e.cfg.BTSwarmBoostConns
		}
		info := mt.t.Info()
		activeStreams := e.streamManager.ActiveStreamsForTorrent(mt.t.InfoHash().HexString())
		blockedReason := decideHardRefreshBlockedReason(e.cfg, hardRefreshGateSnapshot{
			State:             mt.state,
			AddedAt:           mt.addedAt,
			LastHardRefreshAt: mt.lastHardRefreshAt,
			SoftRefreshCount:  mt.softRefreshAttemptsInEpisode,
			ActiveStreams:     activeStreams,
			Pending:           mt.pendingHardRefresh,
			Now:               now,
		})
		mt.nextHardRefreshAt = nextHardRefreshAt(e.cfg, hardRefreshGateSnapshot{AddedAt: mt.addedAt, LastHardRefreshAt: mt.lastHardRefreshAt, Now: now})
		mt.nextClientRecycleAt = nextClientRecycleAt(e.cfg, e.lastClientRecycleAt, now)
		health.Torrents = append(health.Torrents, models.BTTorrentHealth{
			Hash:                            mt.t.InfoHash().HexString(),
			Name:                            mt.t.Name(),
			State:                           mt.state,
			Known:                           stats.TotalPeers,
			Connected:                       stats.ActivePeers,
			Pending:                         stats.PendingPeers,
			HalfOpen:                        stats.HalfOpenPeers,
			Seeds:                           stats.ConnectedSeeders,
			MetadataReady:                   info != nil,
			LastReaddSource:                 mt.lastReaddSource,
			AutoHardRefreshEnabled:          e.cfg.BTSwarmAutoHardRefreshEnabled,
			ClientRecycleAfterSoftFails:     e.cfg.BTClientRecycleAfterSoftFails,
			ClientRecycleMinTorrentAgeSec:   e.cfg.BTClientRecycleMinTorrentAgeSec,
			RecycleScheduledReason:          e.recycleScheduledReason,
			TrackerStatus:                   mt.trackerStatus,
			TrackerError:                    mt.trackerError,
			DownloadSpeed:                   mt.downloadSpeed,
			UploadSpeed:                     mt.uploadSpeed,
			Degraded:                        mt.degraded,
			LastRefreshAt:                   formatBTHealthTime(mt.lastSwarmRefreshAt),
			LastRefreshReason:               mt.lastSwarmRefreshReason,
			LastHealthyAt:                   formatBTHealthTime(mt.lastHealthyAt),
			BoostedUntil:                    formatBTHealthTime(mt.boostUntil),
			MaxEstablishedConns:             maxEstablishedConns,
			PeakConnected:                   mt.peakConnected,
			PeakSeeds:                       mt.peakSeeds,
			PeakDownloadSpeed:               mt.peakDownloadSpeed,
			PeakUpdatedAt:                   formatBTHealthTime(mt.peakUpdatedAt),
			SoftRefreshCount:                mt.softRefreshCount,
			HardRefreshCount:                mt.hardRefreshCount,
			LastHardRefreshAt:               formatBTHealthTime(mt.lastHardRefreshAt),
			LastHardRefreshReason:           mt.lastHardRefreshReason,
			LastHardRefreshError:            mt.lastHardRefreshErr,
			HardRefreshAllowed:              blockedReason == "",
			HardRefreshBlockedReason:        blockedReason,
			ActiveStreams:                   activeStreams,
			DegradationEpisodeStartedAt:     formatBTHealthTime(mt.degradationEpisodeStartedAt),
			LastDegradedAt:                  formatBTHealthTime(mt.lastDegradedAt),
			LastRecoveredAt:                 formatBTHealthTime(mt.lastRecoveredAt),
			LastSoftRefreshAt:               formatBTHealthTime(mt.lastSoftRefreshAt),
			LastSoftRefreshReason:           mt.lastSoftRefreshReason,
			SoftRefreshAttemptsInEpisode:    mt.softRefreshAttemptsInEpisode,
			HardRefreshAttemptsInEpisode:    mt.hardRefreshAttemptsInEpisode,
			LastSoftRefreshCountResetReason: mt.lastSoftRefreshCountResetReason,
			NextHardRefreshAt:               formatBTHealthTime(mt.nextHardRefreshAt),
			NextClientRecycleAt:             formatBTHealthTime(mt.nextClientRecycleAt),
		})
	}

	return health
}

func formatBTHealthTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func normalizedBTClientProfile(profile string) string {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "default":
		return "default"
	default:
		return "qbittorrent"
	}
}

func normalizedRetrackersMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "off", "replace":
		return strings.ToLower(strings.TrimSpace(mode))
	default:
		return "append"
	}
}

func (e *Engine) mapManagedTorrent(mt *ManagedTorrent) *models.Torrent {
	t := mt.t
	info := t.Info()
	size := int64(0)
	if info != nil {
		size = info.TotalLength()
	}

	downloaded := t.BytesCompleted()
	progress := float64(0)
	if size > 0 {
		progress = float64(downloaded) / float64(size) * 100
	}

	stats := t.Stats()
	peerSummary := models.PeerSummary{
		Known:         stats.TotalPeers,
		Connected:     stats.ActivePeers,
		Pending:       stats.PendingPeers,
		HalfOpen:      stats.HalfOpenPeers,
		Seeds:         stats.ConnectedSeeders,
		MetadataReady: info != nil,
		TrackerStatus: mt.trackerStatus,
		TrackerError:  mt.trackerError,
		DHTStatus:     e.dhtStatus(),
	}
	e.updateSpeedsLocked(mt, stats)
	e.logPeerSummaryLocked(t.InfoHash().HexString(), peerSummary, mt)

	model := &models.Torrent{
		Hash:       t.InfoHash().HexString(),
		Name:       t.Name(),
		Size:       size,
		Downloaded: downloaded,
		Progress:   progress,
		State:      mt.state,
		Error:      mt.err,

		DownloadSpeed: mt.downloadSpeed,
		UploadSpeed:   mt.uploadSpeed,
		Peers:         peerSummary.Connected,
		Seeds:         peerSummary.Seeds,
		PeerSummary:   peerSummary,
		SourceURI:     mt.sourceURI,
	}

	if info != nil {
		for i, file := range t.Files() {
			model.Files = append(model.Files, &models.File{
				Index:      i,
				Path:       file.DisplayPath(),
				Size:       file.Length(),
				Downloaded: file.BytesCompleted(),
				Priority:   models.FilePriority(file.Priority()),
				IsMedia:    isMediaFile(file.DisplayPath()),
			})
		}
	}

	return model
}

func isMediaFile(filePath string) bool {
	switch strings.ToLower(filepath.Ext(filePath)) {
	case ".mp4", ".mkv", ".avi", ".mov", ".m4v", ".webm", ".ts":
		return true
	default:
		return false
	}
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func (e *Engine) setStateLocked(hash string, mt *ManagedTorrent, state models.TorrentState, errReason models.ErrorReason, reason string) {
	oldState := mt.state
	oldErr := mt.err
	mt.state = state
	mt.err = errReason
	if oldState != state || oldErr != errReason {
		logging.Infof("torrent state change hash=%s state=%s->%s error=%s reason=%s", hash, oldState, state, errReason, reason)
	}
}

func (e *Engine) watchMetadata(hash string, t *torrent.Torrent) {
	go func() {
		<-t.GotInfo()

		e.mu.Lock()
		defer e.mu.Unlock()

		mt, ok := e.managedTorrents[hash]
		if !ok || mt.t != t || mt.metadataLogged {
			return
		}

		info := t.Info()
		if info == nil {
			return
		}

		fileCount := len(t.Files())
		mi := t.Metainfo()
		mt.metainfo = &mi
		mt.metadataLogged = true
		mt.metadataWaitLogged = false
		logging.Infof("torrent metadata ready hash=%s name=%q size=%d files=%d", hash, t.Name(), info.TotalLength(), fileCount)
		if mt.state == models.StateDownloading && !mt.downloadAllStarted {
			t.DownloadAll()
			mt.downloadAllStarted = true
			logging.Debugf("download all started after metadata hash=%s files=%d", hash, fileCount)
		}
	}()
}

func (e *Engine) updateSpeedsLocked(mt *ManagedTorrent, stats torrent.TorrentStats) {
	now := time.Now()
	readBytes := stats.PeerConns.BytesReadUsefulData.Int64() + stats.WebSeeds.BytesReadUsefulData.Int64()
	writtenBytes := stats.PeerConns.BytesWrittenData.Int64() + stats.WebSeeds.BytesWrittenData.Int64()

	if !mt.lastStatsAt.IsZero() {
		elapsed := now.Sub(mt.lastStatsAt).Seconds()
		if elapsed > 0 {
			readDelta := readBytes - mt.lastReadBytes
			writtenDelta := writtenBytes - mt.lastWrittenBytes
			if readDelta < 0 {
				readDelta = 0
			}
			if writtenDelta < 0 {
				writtenDelta = 0
			}
			mt.downloadSpeed = int64(float64(readDelta) / elapsed)
			mt.uploadSpeed = int64(float64(writtenDelta) / elapsed)
		}
	}

	mt.lastStatsAt = now
	mt.lastReadBytes = readBytes
	mt.lastWrittenBytes = writtenBytes
}

func (e *Engine) logPeerSummaryLocked(hash string, summary models.PeerSummary, mt *ManagedTorrent) {
	if !logging.IsDebugEnabled() {
		return
	}

	now := time.Now()
	changed := summary != mt.lastPeerSummary
	periodic := mt.lastPeerLog.IsZero() || now.Sub(mt.lastPeerLog) >= 30*time.Second
	if !changed && !periodic {
		return
	}

	logging.Debugf("peer summary hash=%s known=%d connected=%d pending=%d half_open=%d seeds=%d metadata_ready=%t tracker_status=%s dht_status=%s download_speed=%d upload_speed=%d",
		hash,
		summary.Known,
		summary.Connected,
		summary.Pending,
		summary.HalfOpen,
		summary.Seeds,
		summary.MetadataReady,
		summary.TrackerStatus,
		summary.DHTStatus,
		mt.downloadSpeed,
		mt.uploadSpeed,
	)
	mt.lastPeerSummary = summary
	mt.lastPeerLog = now
}

func (e *Engine) dhtStatus() string {
	if len(e.client.DhtServers()) == 0 {
		return "disabled"
	}
	return "enabled"
}

func (e *Engine) handleStatusEvent(event torrent.StatusUpdatedEvent) {
	hash := strings.ToLower(event.InfoHash)
	tracker := logging.SafeURLSummary(event.Url)
	if event.Error != nil {
		logging.Warnf("torrent status event=%s hash=%s tracker=%s error=%v", event.Event, hash, tracker, event.Error)
	} else {
		logging.Debugf("torrent status event=%s hash=%s tracker=%s", event.Event, hash, tracker)
	}

	if hash == "" {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	mt, ok := e.managedTorrents[hash]
	if !ok {
		return
	}

	switch event.Event {
	case torrent.TrackerConnected, torrent.TrackerAnnounceSuccessful:
		mt.trackerStatus = string(event.Event)
		mt.trackerError = ""
	case torrent.TrackerDisconnected:
		mt.trackerStatus = string(event.Event)
	case torrent.TrackerAnnounceError:
		mt.trackerStatus = string(event.Event)
		if event.Error != nil {
			mt.trackerError = logging.SanitizeText(event.Error.Error())
		}
	}
}

func (e *Engine) logPeerConnEvent(event string, conn *torrent.PeerConn) {
	if conn == nil || conn.Torrent() == nil {
		return
	}

	t := conn.Torrent()
	logging.Debugf("peer connection %s hash=%s source=%s", event, t.InfoHash().HexString(), conn.Discovery)
}

// resourceMonitor checks disk space and manages active downloads limit
func (e *Engine) resourceMonitor() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	e.mu.Lock()
	e.manageResourcesLocked()
	e.mu.Unlock()

	for range ticker.C {
		e.mu.Lock()
		e.manageResourcesLocked()
		e.mu.Unlock()
	}
}

type swarmSnapshot struct {
	State             models.TorrentState
	MetadataReady     bool
	Complete          bool
	Connected         int
	Seeds             int
	DownloadSpeed     int64
	PeakConnected     int
	PeakSeeds         int
	PeakDownloadSpeed int64
	PeakUpdatedAt     time.Time
	Now               time.Time
}

type swarmDecision struct {
	Degraded bool
	Reason   string
}

func decideSwarmHealth(cfg *config.Config, snap swarmSnapshot, stallStartedAt time.Time) swarmDecision {
	if cfg == nil {
		cfg = &config.Config{}
	}
	config.ApplyDefaults(cfg)

	switch snap.State {
	case models.StatePaused, models.StateError, models.StateMissingFiles, models.StateDiskFull:
		return swarmDecision{}
	}

	if !snap.MetadataReady {
		if snap.Connected < cfg.BTSwarmMinConnectedPeers {
			return swarmDecision{Degraded: true, Reason: "metadata pending with low connected peers"}
		}
		return swarmDecision{}
	}

	if !snap.Complete && snap.Seeds < cfg.BTSwarmMinConnectedSeeds {
		return swarmDecision{Degraded: true, Reason: "connected seeds below threshold"}
	}
	if snap.Connected < cfg.BTSwarmMinConnectedPeers {
		return swarmDecision{Degraded: true, Reason: "connected peers below threshold"}
	}
	if !snap.Complete && snap.DownloadSpeed < int64(cfg.BTSwarmStalledSpeedBps) && !stallStartedAt.IsZero() && snap.Now.Sub(stallStartedAt) >= time.Duration(cfg.BTSwarmStalledDurationSec)*time.Second {
		return swarmDecision{Degraded: true, Reason: "download speed stalled"}
	}

	peakValid := !snap.PeakUpdatedAt.IsZero() && snap.Now.Sub(snap.PeakUpdatedAt) <= time.Duration(cfg.BTSwarmPeakTTLSec)*time.Second
	if peakValid && snap.PeakConnected > cfg.BTSwarmMinConnectedPeers && snap.Connected < int(float64(snap.PeakConnected)*cfg.BTSwarmPeerDropRatio) {
		return swarmDecision{Degraded: true, Reason: "connected peers dropped below recent peak"}
	}
	if peakValid && !snap.Complete && snap.PeakSeeds > cfg.BTSwarmMinConnectedSeeds && snap.Seeds < int(float64(snap.PeakSeeds)*cfg.BTSwarmSeedDropRatio) {
		return swarmDecision{Degraded: true, Reason: "connected seeds dropped below recent peak"}
	}
	if peakValid && !snap.Complete && snap.PeakDownloadSpeed > int64(cfg.BTSwarmStalledSpeedBps) && snap.DownloadSpeed < int64(float64(snap.PeakDownloadSpeed)*cfg.BTSwarmSpeedDropRatio) {
		return swarmDecision{Degraded: true, Reason: "download speed dropped below recent peak"}
	}

	return swarmDecision{}
}

type hardRefreshGateSnapshot struct {
	State             models.TorrentState
	AddedAt           time.Time
	LastHardRefreshAt time.Time
	SoftRefreshCount  int
	ActiveStreams     int
	Pending           bool
	Manual            bool
	Now               time.Time
}

func decideHardRefreshBlockedReason(cfg *config.Config, snap hardRefreshGateSnapshot) string {
	if cfg == nil {
		cfg = &config.Config{}
	}
	config.ApplyDefaults(cfg)
	if snap.ActiveStreams > 0 {
		return "active stream"
	}
	if !cfg.BTSwarmHardRefreshEnabled {
		return "disabled"
	}
	switch snap.State {
	case models.StatePaused, models.StateError, models.StateMissingFiles, models.StateDiskFull:
		return "state " + string(snap.State)
	}
	if snap.Pending {
		return "pending"
	}
	if !snap.LastHardRefreshAt.IsZero() && snap.Now.Sub(snap.LastHardRefreshAt) < time.Duration(cfg.BTSwarmHardRefreshCooldownSec)*time.Second {
		return "cooldown"
	}
	if !snap.AddedAt.IsZero() && snap.Now.Sub(snap.AddedAt) < time.Duration(cfg.BTSwarmHardRefreshMinTorrentAgeSec)*time.Second {
		return "torrent too young"
	}
	if !snap.Manual && snap.SoftRefreshCount < cfg.BTSwarmHardRefreshAfterSoftFails {
		return "waiting for soft refresh attempts"
	}
	return ""
}

func nextHardRefreshAt(cfg *config.Config, snap hardRefreshGateSnapshot) time.Time {
	config.ApplyDefaults(cfg)
	var next time.Time
	if !snap.LastHardRefreshAt.IsZero() {
		next = snap.LastHardRefreshAt.Add(time.Duration(cfg.BTSwarmHardRefreshCooldownSec) * time.Second)
	}
	if !snap.AddedAt.IsZero() {
		ageNext := snap.AddedAt.Add(time.Duration(cfg.BTSwarmHardRefreshMinTorrentAgeSec) * time.Second)
		if next.IsZero() || ageNext.After(next) {
			next = ageNext
		}
	}
	if !next.IsZero() && next.Before(snap.Now) {
		return time.Time{}
	}
	return next
}

func (e *Engine) swarmRefreshMonitor() {
	interval := time.Duration(e.cfg.BTSwarmCheckIntervalSec) * time.Second
	if interval <= 0 {
		interval = time.Minute
	}
	logging.Infof("swarm refresh monitor started interval=%s", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		e.checkSwarms()
	}
}

func (e *Engine) checkSwarms() {
	now := time.Now()
	type refreshTarget struct {
		hash        string
		t           *torrent.Torrent
		reason      string
		downloadAll bool
	}
	type boostExpireTarget struct {
		hash string
		t    *torrent.Torrent
		max  int
	}
	var targets []refreshTarget
	var hardRefreshTargets []refreshTarget
	var expiredBoosts []boostExpireTarget
	recycleReason := ""

	e.mu.Lock()
	e.recycleScheduledReason = ""
	for hash, mt := range e.managedTorrents {
		stats := mt.t.Stats()
		e.updateSpeedsLocked(mt, stats)
		updateSwarmPeaksLocked(e.cfg, mt, stats.ActivePeers, stats.ConnectedSeeders, mt.downloadSpeed, now)
		info := mt.t.Info()
		complete := info != nil && mt.t.BytesCompleted() == info.TotalLength()

		if !complete && mt.downloadSpeed < int64(e.cfg.BTSwarmStalledSpeedBps) {
			if mt.stallStartedAt.IsZero() {
				mt.stallStartedAt = now
			}
		} else {
			mt.stallStartedAt = time.Time{}
		}

		decision := decideSwarmHealth(e.cfg, swarmSnapshot{
			State:             mt.state,
			MetadataReady:     info != nil,
			Complete:          complete,
			Connected:         stats.ActivePeers,
			Seeds:             stats.ConnectedSeeders,
			DownloadSpeed:     mt.downloadSpeed,
			PeakConnected:     mt.peakConnected,
			PeakSeeds:         mt.peakSeeds,
			PeakDownloadSpeed: mt.peakDownloadSpeed,
			PeakUpdatedAt:     mt.peakUpdatedAt,
			Now:               now,
		}, mt.stallStartedAt)

		mt.lastSwarmCheckAt = now
		if decision.Degraded {
			updateDegradationEpisodeLocked(e.cfg, mt, true, now)
			if !mt.degraded || mt.lastSwarmRefreshReason != decision.Reason {
				logging.Infof("swarm degraded hash=%s reason=%s connected=%d seeds=%d speed=%d", hash, decision.Reason, stats.ActivePeers, stats.ConnectedSeeders, mt.downloadSpeed)
			}
			mt.degraded = true
			if mt.softRefreshAttemptsInEpisode >= e.cfg.BTClientRecycleAfterSoftFails && recycleReason == "" {
				if !mt.addedAt.IsZero() && now.Sub(mt.addedAt) < time.Duration(e.cfg.BTClientRecycleMinTorrentAgeSec)*time.Second {
					mt.nextClientRecycleAt = mt.addedAt.Add(time.Duration(e.cfg.BTClientRecycleMinTorrentAgeSec) * time.Second)
					e.recycleScheduledReason = "client recycle blocked: torrent too young"
				} else if blocked := e.clientRecycleBlockedReasonLocked(now); blocked == "" {
					recycleReason = "degraded after soft refresh: " + decision.Reason
					mt.nextClientRecycleAt = now
					e.recycleScheduledReason = recycleReason
					logging.Infof("bt client recycle scheduled hash=%s reason=%s soft_attempts=%d", hash, decision.Reason, mt.softRefreshAttemptsInEpisode)
				} else {
					mt.nextClientRecycleAt = nextClientRecycleAt(e.cfg, e.lastClientRecycleAt, now)
					e.recycleScheduledReason = "client recycle blocked: " + blocked
				}
			}
			activeStreams := e.streamManager.ActiveStreamsForTorrent(hash)
			mt.nextHardRefreshAt = nextHardRefreshAt(e.cfg, hardRefreshGateSnapshot{AddedAt: mt.addedAt, LastHardRefreshAt: mt.lastHardRefreshAt, Now: now})
			if e.cfg.BTSwarmAutoHardRefreshEnabled {
				blockedReason := decideHardRefreshBlockedReason(e.cfg, hardRefreshGateSnapshot{
					State:             mt.state,
					AddedAt:           mt.addedAt,
					LastHardRefreshAt: mt.lastHardRefreshAt,
					SoftRefreshCount:  mt.softRefreshAttemptsInEpisode,
					ActiveStreams:     activeStreams,
					Pending:           mt.pendingHardRefresh,
					Now:               now,
				})
				if blockedReason == "" && mt.metainfo != nil {
					mt.pendingHardRefresh = true
					hardRefreshTargets = append(hardRefreshTargets, refreshTarget{hash: hash, t: mt.t, reason: decision.Reason})
					logging.Infof("swarm hard refresh scheduled hash=%s reason=%s soft_refresh_count=%d", hash, decision.Reason, mt.softRefreshCount)
					continue
				}
			}

			if mt.lastSwarmRefreshAt.IsZero() || now.Sub(mt.lastSwarmRefreshAt) >= time.Duration(e.cfg.BTSwarmRefreshCooldownSec)*time.Second {
				mt.lastSwarmRefreshAt = now
				mt.lastSwarmRefreshReason = decision.Reason
				mt.lastSoftRefreshAt = now
				mt.lastSoftRefreshReason = decision.Reason
				mt.softRefreshCount++
				mt.softRefreshAttemptsInEpisode++
				mt.boostUntil = now.Add(time.Duration(e.cfg.BTSwarmBoostDurationSec) * time.Second)
				if mt.normalMaxEstablishedConns == 0 {
					mt.normalMaxEstablishedConns = e.cfg.BTEstablishedConns
				}
				downloadAll := info != nil && !complete
				if downloadAll {
					mt.downloadAllStarted = true
				}
				targets = append(targets, refreshTarget{hash: hash, t: mt.t, reason: decision.Reason, downloadAll: downloadAll})
				logging.Infof("swarm refresh scheduled hash=%s reason=%s boost_conns=%d boost_until=%s", hash, decision.Reason, e.cfg.BTSwarmBoostConns, mt.boostUntil.Format(time.RFC3339))
			}
			continue
		}

		mt.lastHealthyAt = now
		updateDegradationEpisodeLocked(e.cfg, mt, false, now)
		if mt.degraded {
			logging.Infof("swarm recovered hash=%s connected=%d seeds=%d speed=%d", hash, stats.ActivePeers, stats.ConnectedSeeders, mt.downloadSpeed)
		}
		mt.degraded = false
		if !mt.boostUntil.IsZero() && now.After(mt.boostUntil) {
			if mt.normalMaxEstablishedConns > 0 {
				expiredBoosts = append(expiredBoosts, boostExpireTarget{hash: hash, t: mt.t, max: mt.normalMaxEstablishedConns})
			}
			logging.Debugf("swarm boost expired hash=%s", hash)
			mt.boostUntil = time.Time{}
		}
	}
	e.mu.Unlock()

	for _, target := range targets {
		target.t.SetMaxEstablishedConns(e.cfg.BTSwarmBoostConns)
		target.t.AllowDataDownload()
		if !e.cfg.BTNoUpload {
			target.t.AllowDataUpload()
		}
		if target.downloadAll {
			target.t.DownloadAll()
		}
		e.refreshDHTAsync(target.hash, target.t, target.reason)
	}
	for _, target := range hardRefreshTargets {
		if err := e.hardRefreshTorrent(target.hash, target.reason, false); err != nil {
			logging.Warnf("swarm hard refresh failed hash=%s reason=%s: %v", target.hash, target.reason, err)
		}
	}
	if recycleReason != "" {
		if err := e.recycleClient(recycleReason, false); err != nil {
			logging.Warnf("bt client recycle failed reason=%s: %v", recycleReason, err)
		}
	}
	for _, target := range expiredBoosts {
		target.t.SetMaxEstablishedConns(target.max)
	}
}

func updateSwarmPeaksLocked(cfg *config.Config, mt *ManagedTorrent, connected, seeds int, downloadSpeed int64, now time.Time) {
	config.ApplyDefaults(cfg)
	if mt.peakUpdatedAt.IsZero() || now.Sub(mt.peakUpdatedAt) > time.Duration(cfg.BTSwarmPeakTTLSec)*time.Second {
		mt.peakConnected = connected
		mt.peakSeeds = seeds
		mt.peakDownloadSpeed = downloadSpeed
		mt.peakUpdatedAt = now
		return
	}
	updated := false
	if connected > mt.peakConnected {
		mt.peakConnected = connected
		updated = true
	}
	if seeds > mt.peakSeeds {
		mt.peakSeeds = seeds
		updated = true
	}
	if downloadSpeed > mt.peakDownloadSpeed {
		mt.peakDownloadSpeed = downloadSpeed
		updated = true
	}
	if updated {
		mt.peakUpdatedAt = now
	}
}

func updateDegradationEpisodeLocked(cfg *config.Config, mt *ManagedTorrent, degraded bool, now time.Time) {
	config.ApplyDefaults(cfg)
	if degraded {
		mt.lastDegradedAt = now
		if mt.degradationEpisodeStartedAt.IsZero() || now.Sub(mt.degradationEpisodeStartedAt) > time.Duration(cfg.BTSwarmDegradationEpisodeTTLSec)*time.Second {
			mt.degradationEpisodeStartedAt = now
			mt.softRefreshAttemptsInEpisode = 0
			mt.hardRefreshAttemptsInEpisode = 0
			mt.lastSoftRefreshCountResetReason = "new degradation episode"
		}
		return
	}

	mt.lastRecoveredAt = now
	if mt.degradationEpisodeStartedAt.IsZero() {
		return
	}
	if mt.lastDegradedAt.IsZero() || now.Sub(mt.lastDegradedAt) < time.Duration(cfg.BTSwarmRecoveryGraceSec)*time.Second {
		return
	}
	mt.degradationEpisodeStartedAt = time.Time{}
	mt.softRefreshAttemptsInEpisode = 0
	mt.hardRefreshAttemptsInEpisode = 0
	mt.softRefreshCount = 0
	mt.nextHardRefreshAt = time.Time{}
	mt.nextClientRecycleAt = time.Time{}
	mt.lastSoftRefreshCountResetReason = "stable recovery"
}

func (e *Engine) HardRefresh(hash string, reason string) error {
	return e.hardRefreshTorrent(hash, reason, true)
}

func (e *Engine) hardRefreshTorrent(hash string, reason string, manual bool) error {
	now := time.Now()

	e.mu.Lock()
	mt, ok := e.managedTorrents[hash]
	if !ok {
		e.mu.Unlock()
		return TorrentNotFoundError{Hash: hash}
	}
	activeStreams := e.streamManager.ActiveStreamsForTorrent(hash)
	if blocked := decideHardRefreshBlockedReason(e.cfg, hardRefreshGateSnapshot{
		State:             mt.state,
		AddedAt:           mt.addedAt,
		LastHardRefreshAt: mt.lastHardRefreshAt,
		SoftRefreshCount:  mt.softRefreshAttemptsInEpisode,
		ActiveStreams:     activeStreams,
		Pending:           false,
		Manual:            manual,
		Now:               now,
	}); blocked != "" {
		mt.pendingHardRefresh = false
		e.mu.Unlock()
		return fmt.Errorf("hard refresh blocked: %s", blocked)
	}

	spec, readdSource, err := e.torrentSpecForReadd(mt, hash)
	if err != nil {
		mt.pendingHardRefresh = false
		mt.lastHardRefreshAt = now
		mt.lastHardRefreshReason = reason
		mt.lastHardRefreshErr = logging.SanitizeText(err.Error())
		e.mu.Unlock()
		return err
	}
	sourceURI := strings.TrimSpace(mt.sourceURI)
	if sourceURI == "" {
		sourceURI = "magnet:?xt=urn:btih:" + hash
	}
	saved := hardRefreshSavedState{
		state:                        mt.state,
		err:                          mt.err,
		sourceURI:                    sourceURI,
		metainfo:                     cloneMetaInfo(mt.metainfo),
		lastReaddSource:              readdSource,
		metadataLogged:               mt.metadataLogged,
		downloadAllStarted:           mt.downloadAllStarted,
		lastHealthyAt:                mt.lastHealthyAt,
		peakConnected:                mt.peakConnected,
		peakSeeds:                    mt.peakSeeds,
		peakDownloadSpeed:            mt.peakDownloadSpeed,
		peakUpdatedAt:                mt.peakUpdatedAt,
		softRefreshCount:             mt.softRefreshCount,
		hardRefreshCount:             mt.hardRefreshCount,
		hardRefreshAttemptsInEpisode: mt.hardRefreshAttemptsInEpisode,
		normalMaxEstablishedConns:    mt.normalMaxEstablishedConns,
	}
	oldTorrent := mt.t
	delete(e.managedTorrents, hash)
	e.mu.Unlock()

	logging.Infof("hard refreshing torrent hash=%s reason=%s readd_source=%s", hash, reason, readdSource)
	oldTorrent.Drop()
	newTorrent, _, err := e.client.AddTorrentSpec(spec)
	if err != nil {
		e.mu.Lock()
		failed := &ManagedTorrent{
			t:                            oldTorrent,
			state:                        models.StateError,
			err:                          models.ErrTrackerUnreachable,
			sourceURI:                    saved.sourceURI,
			metainfo:                     cloneMetaInfo(saved.metainfo),
			lastReaddSource:              saved.lastReaddSource,
			addedAt:                      now,
			lastHealthyAt:                saved.lastHealthyAt,
			peakConnected:                saved.peakConnected,
			peakSeeds:                    saved.peakSeeds,
			peakDownloadSpeed:            saved.peakDownloadSpeed,
			peakUpdatedAt:                saved.peakUpdatedAt,
			softRefreshCount:             saved.softRefreshCount,
			hardRefreshCount:             saved.hardRefreshCount,
			hardRefreshAttemptsInEpisode: saved.hardRefreshAttemptsInEpisode + 1,
			lastHardRefreshAt:            now,
			lastHardRefreshReason:        reason,
			lastHardRefreshErr:           logging.SanitizeText(err.Error()),
			normalMaxEstablishedConns:    saved.normalMaxEstablishedConns,
		}
		if _, exists := e.managedTorrents[hash]; !exists {
			e.managedTorrents[hash] = failed
		}
		e.mu.Unlock()
		return err
	}

	newMT := &ManagedTorrent{
		t:                            newTorrent,
		state:                        saved.state,
		err:                          saved.err,
		sourceURI:                    saved.sourceURI,
		metainfo:                     cloneMetaInfo(saved.metainfo),
		lastReaddSource:              saved.lastReaddSource,
		addedAt:                      now,
		metadataLogged:               saved.metadataLogged,
		downloadAllStarted:           false,
		lastHealthyAt:                saved.lastHealthyAt,
		peakConnected:                saved.peakConnected,
		peakSeeds:                    saved.peakSeeds,
		peakDownloadSpeed:            saved.peakDownloadSpeed,
		peakUpdatedAt:                saved.peakUpdatedAt,
		softRefreshCount:             0,
		hardRefreshCount:             saved.hardRefreshCount + 1,
		hardRefreshAttemptsInEpisode: saved.hardRefreshAttemptsInEpisode + 1,
		lastHardRefreshAt:            now,
		lastHardRefreshReason:        reason,
		normalMaxEstablishedConns:    saved.normalMaxEstablishedConns,
	}
	if newMT.normalMaxEstablishedConns == 0 {
		newMT.normalMaxEstablishedConns = e.cfg.BTEstablishedConns
	}
	newHash := newTorrent.InfoHash().HexString()
	if newHash != hash {
		return fmt.Errorf("hard refresh hash mismatch: %s != %s", newHash, hash)
	}

	e.mu.Lock()
	if _, exists := e.managedTorrents[hash]; !exists {
		e.managedTorrents[hash] = newMT
		e.manageResourcesLocked()
	}
	e.mu.Unlock()
	if newTorrent.Info() == nil {
		e.watchMetadata(hash, newTorrent)
	}
	return nil
}

type hardRefreshSavedState struct {
	state                        models.TorrentState
	err                          models.ErrorReason
	sourceURI                    string
	metainfo                     *metainfo.MetaInfo
	lastReaddSource              string
	metadataLogged               bool
	downloadAllStarted           bool
	lastHealthyAt                time.Time
	peakConnected                int
	peakSeeds                    int
	peakDownloadSpeed            int64
	peakUpdatedAt                time.Time
	softRefreshCount             int
	hardRefreshCount             int
	hardRefreshAttemptsInEpisode int
	normalMaxEstablishedConns    int
}

func (e *Engine) torrentSpecFromSource(sourceURI string) (*torrent.TorrentSpec, error) {
	spec, err := torrent.TorrentSpecFromMagnetUri(sourceURI)
	if err != nil {
		return nil, err
	}
	e.augmentTorrentSpec(spec)
	return spec, nil
}

func (e *Engine) torrentSpecForReadd(mt *ManagedTorrent, hash string) (*torrent.TorrentSpec, string, error) {
	if mt != nil {
		if mt.metainfo != nil {
			spec, err := torrent.TorrentSpecFromMetaInfoErr(cloneMetaInfo(mt.metainfo))
			if err != nil {
				return nil, "", err
			}
			e.augmentTorrentSpec(spec)
			return spec, "metainfo", nil
		}
		if mt.t != nil && mt.t.Info() != nil {
			mi := mt.t.Metainfo()
			spec, err := torrent.TorrentSpecFromMetaInfoErr(&mi)
			if err != nil {
				return nil, "", err
			}
			e.augmentTorrentSpec(spec)
			mt.metainfo = &mi
			return spec, "metainfo", nil
		}
		sourceURI := strings.TrimSpace(mt.sourceURI)
		if sourceURI != "" {
			spec, err := e.torrentSpecFromSource(sourceURI)
			if err != nil {
				return nil, "", err
			}
			return spec, "magnet", nil
		}
	}
	if strings.TrimSpace(hash) == "" {
		return nil, "", fmt.Errorf("missing torrent hash for re-add")
	}
	spec, err := e.torrentSpecFromSource("magnet:?xt=urn:btih:" + strings.TrimSpace(hash))
	if err != nil {
		return nil, "", err
	}
	logging.Warnf("re-add falling back to bare info hash hash=%s", strings.TrimSpace(hash))
	return spec, "infohash", nil
}

func cloneMetaInfo(mi *metainfo.MetaInfo) *metainfo.MetaInfo {
	if mi == nil {
		return nil
	}
	clone := *mi
	return &clone
}

type recycleTorrentSnapshot struct {
	hash                         string
	sourceURI                    string
	metainfo                     *metainfo.MetaInfo
	lastReaddSource              string
	state                        models.TorrentState
	err                          models.ErrorReason
	metadataLogged               bool
	lastHealthyAt                time.Time
	peakConnected                int
	peakSeeds                    int
	peakDownloadSpeed            int64
	peakUpdatedAt                time.Time
	softRefreshCount             int
	hardRefreshCount             int
	degradationEpisodeStartedAt  time.Time
	lastDegradedAt               time.Time
	lastRecoveredAt              time.Time
	lastSoftRefreshAt            time.Time
	lastSoftRefreshReason        string
	softRefreshAttemptsInEpisode int
	hardRefreshAttemptsInEpisode int
	lastHardRefreshAt            time.Time
	lastHardRefreshReason        string
	lastHardRefreshErr           string
	normalMaxEstablishedConns    int
}

func (e *Engine) RecycleClient(reason string) error {
	return e.recycleClient(reason, true)
}

func (e *Engine) recycleClient(reason string, manual bool) error {
	now := time.Now()
	e.mu.Lock()
	if blocked := e.clientRecycleBlockedReasonLocked(now); blocked != "" {
		e.mu.Unlock()
		return fmt.Errorf("client recycle blocked: %s", blocked)
	}
	e.recyclingClient = true
	oldClient := e.client
	snapshots := make([]recycleTorrentSnapshot, 0, len(e.managedTorrents))
	for hash, mt := range e.managedTorrents {
		sourceURI := strings.TrimSpace(mt.sourceURI)
		if sourceURI == "" {
			sourceURI = "magnet:?xt=urn:btih:" + hash
		}
		snapshots = append(snapshots, recycleTorrentSnapshot{
			hash:                         hash,
			sourceURI:                    sourceURI,
			metainfo:                     cloneMetaInfo(mt.metainfo),
			lastReaddSource:              mt.lastReaddSource,
			state:                        mt.state,
			err:                          mt.err,
			metadataLogged:               mt.metadataLogged,
			lastHealthyAt:                mt.lastHealthyAt,
			peakConnected:                mt.peakConnected,
			peakSeeds:                    mt.peakSeeds,
			peakDownloadSpeed:            mt.peakDownloadSpeed,
			peakUpdatedAt:                mt.peakUpdatedAt,
			softRefreshCount:             mt.softRefreshCount,
			hardRefreshCount:             mt.hardRefreshCount,
			degradationEpisodeStartedAt:  mt.degradationEpisodeStartedAt,
			lastDegradedAt:               mt.lastDegradedAt,
			lastRecoveredAt:              mt.lastRecoveredAt,
			lastSoftRefreshAt:            mt.lastSoftRefreshAt,
			lastSoftRefreshReason:        mt.lastSoftRefreshReason,
			softRefreshAttemptsInEpisode: mt.softRefreshAttemptsInEpisode,
			hardRefreshAttemptsInEpisode: mt.hardRefreshAttemptsInEpisode,
			lastHardRefreshAt:            mt.lastHardRefreshAt,
			lastHardRefreshReason:        mt.lastHardRefreshReason,
			lastHardRefreshErr:           mt.lastHardRefreshErr,
			normalMaxEstablishedConns:    mt.normalMaxEstablishedConns,
		})
	}
	e.mu.Unlock()

	newClient, err := e.newTorrentClient()
	if err != nil {
		e.mu.Lock()
		e.recyclingClient = false
		e.lastClientRecycleAt = now
		e.lastClientRecycleReason = reason
		e.lastClientRecycleErr = logging.SanitizeText(err.Error())
		e.mu.Unlock()
		return err
	}

	newManaged := make(map[string]*ManagedTorrent, len(snapshots))
	var watchList []struct {
		hash string
		t    *torrent.Torrent
	}
	var recycleErr error
	for _, snap := range snapshots {
		spec, readdSource, err := e.torrentSpecForReadd(&ManagedTorrent{sourceURI: snap.sourceURI, metainfo: cloneMetaInfo(snap.metainfo)}, snap.hash)
		if err != nil {
			recycleErr = errors.Join(recycleErr, fmt.Errorf("build spec %s: %w", snap.hash, err))
			continue
		}
		newTorrent, _, err := newClient.AddTorrentSpec(spec)
		if err != nil {
			recycleErr = errors.Join(recycleErr, fmt.Errorf("add torrent %s: %w", snap.hash, err))
			continue
		}
		mt := &ManagedTorrent{
			t:                            newTorrent,
			state:                        snap.state,
			err:                          snap.err,
			sourceURI:                    snap.sourceURI,
			metainfo:                     cloneMetaInfo(snap.metainfo),
			lastReaddSource:              readdSource,
			addedAt:                      now,
			metadataLogged:               snap.metadataLogged,
			lastHealthyAt:                snap.lastHealthyAt,
			peakConnected:                snap.peakConnected,
			peakSeeds:                    snap.peakSeeds,
			peakDownloadSpeed:            snap.peakDownloadSpeed,
			peakUpdatedAt:                snap.peakUpdatedAt,
			softRefreshCount:             snap.softRefreshCount,
			hardRefreshCount:             snap.hardRefreshCount,
			degradationEpisodeStartedAt:  snap.degradationEpisodeStartedAt,
			lastDegradedAt:               snap.lastDegradedAt,
			lastRecoveredAt:              snap.lastRecoveredAt,
			lastSoftRefreshAt:            snap.lastSoftRefreshAt,
			lastSoftRefreshReason:        snap.lastSoftRefreshReason,
			softRefreshAttemptsInEpisode: snap.softRefreshAttemptsInEpisode,
			hardRefreshAttemptsInEpisode: snap.hardRefreshAttemptsInEpisode,
			lastHardRefreshAt:            snap.lastHardRefreshAt,
			lastHardRefreshReason:        snap.lastHardRefreshReason,
			lastHardRefreshErr:           snap.lastHardRefreshErr,
			normalMaxEstablishedConns:    snap.normalMaxEstablishedConns,
		}
		if mt.normalMaxEstablishedConns == 0 {
			mt.normalMaxEstablishedConns = e.cfg.BTEstablishedConns
		}
		newHash := newTorrent.InfoHash().HexString()
		newManaged[newHash] = mt
		if newTorrent.Info() == nil {
			watchList = append(watchList, struct {
				hash string
				t    *torrent.Torrent
			}{hash: newHash, t: newTorrent})
		}
	}

	e.mu.Lock()
	e.client = newClient
	e.managedTorrents = newManaged
	e.recyclingClient = false
	e.clientRecycleCount++
	e.clientRecycleTimes = append(e.clientRecycleTimes, now)
	e.lastClientRecycleAt = now
	e.lastClientRecycleReason = reason
	e.lastClientRecycleErr = ""
	if recycleErr != nil {
		e.lastClientRecycleErr = logging.SanitizeText(recycleErr.Error())
	}
	e.manageResourcesLocked()
	e.mu.Unlock()

	if oldClient != nil {
		oldClient.Close()
	}
	for _, item := range watchList {
		e.watchMetadata(item.hash, item.t)
	}
	logging.Infof("bt client recycled reason=%s torrents=%d manual=%t partial_error=%t", reason, len(watchList), manual, recycleErr != nil)
	return recycleErr
}

func (e *Engine) clientRecycleBlockedReasonLocked(now time.Time) string {
	if e.streamManager != nil && e.streamManager.ActiveStreamsTotal() > 0 {
		return "active stream"
	}
	if !e.cfg.BTClientRecycleEnabled {
		return "disabled"
	}
	if e.recyclingClient {
		return "pending"
	}
	if len(e.managedTorrents) < e.cfg.BTClientRecycleMinTorrents {
		return "not enough torrents"
	}
	if !e.lastClientRecycleAt.IsZero() && now.Sub(e.lastClientRecycleAt) < time.Duration(e.cfg.BTClientRecycleCooldownSec)*time.Second {
		return "cooldown"
	}
	if e.clientRecycleCountLastHourLocked(now) >= e.cfg.BTClientRecycleMaxPerHour {
		return "hourly limit"
	}
	return ""
}

func (e *Engine) clientRecycleCountLastHourLocked(now time.Time) int {
	count := 0
	cutoff := now.Add(-time.Hour)
	kept := e.clientRecycleTimes[:0]
	for _, ts := range e.clientRecycleTimes {
		if ts.After(cutoff) {
			kept = append(kept, ts)
			count++
		}
	}
	e.clientRecycleTimes = kept
	return count
}

func nextClientRecycleAt(cfg *config.Config, last time.Time, now time.Time) time.Time {
	config.ApplyDefaults(cfg)
	if last.IsZero() {
		return time.Time{}
	}
	next := last.Add(time.Duration(cfg.BTClientRecycleCooldownSec) * time.Second)
	if next.Before(now) {
		return time.Time{}
	}
	return next
}

func (e *Engine) refreshDHTAsync(hash string, t *torrent.Torrent, reason string) {
	servers := e.client.DhtServers()
	if len(servers) == 0 {
		logging.Debugf("swarm refresh skipped dht announce hash=%s reason=%s dht_servers=0", hash, reason)
		return
	}
	for _, server := range servers {
		server := server
		go func() {
			done, stop, err := t.AnnounceToDht(server)
			if err != nil {
				logging.Warnf("swarm dht announce failed hash=%s reason=%s: %v", hash, reason, err)
				return
			}
			defer stop()
			select {
			case <-done:
				logging.Debugf("swarm dht announce completed hash=%s reason=%s", hash, reason)
			case <-time.After(30 * time.Second):
				logging.Debugf("swarm dht announce timeboxed hash=%s reason=%s", hash, reason)
			}
		}()
	}
}

func (e *Engine) manageResourcesLocked() {
	minFreeBytes := uint64(e.cfg.MinFreeSpaceGB) * 1024 * 1024 * 1024

	freeSpace, err := GetFreeSpace(e.cfg.DownloadDir)
	if err != nil {
		logging.Warnf("failed to check free disk space dir=%s: %v", e.cfg.DownloadDir, err)
	}
	diskFull := err == nil && freeSpace < minFreeBytes

	activeCount := 0

	for hash, mt := range e.managedTorrents {
		if diskFull {
			if mt.state == models.StateDownloading {
				e.setStateLocked(hash, mt, models.StateDiskFull, models.ErrDiskFull, "free disk space below limit")
				mt.downloadAllStarted = false
				if mt.t.Info() != nil {
					for _, f := range mt.t.Files() {
						f.SetPriority(torrent.PiecePriorityNone)
					}
				}
			}
			continue
		}

		// If disk space recovered, queued/diskfull can be started
		if mt.state == models.StateDiskFull {
			e.setStateLocked(hash, mt, models.StateQueued, models.ErrNone, "free disk space recovered")
		}

		if mt.state == models.StateDownloading {
			activeCount++
			if info := mt.t.Info(); info != nil {
				if !mt.downloadAllStarted {
					mt.t.DownloadAll()
					mt.downloadAllStarted = true
					logging.Debugf("download all started hash=%s files=%d", hash, len(mt.t.Files()))
				}
				// Check if finished
				if mt.t.BytesCompleted() == info.TotalLength() {
					e.setStateLocked(hash, mt, models.StateSeeding, models.ErrNone, "download complete")
					activeCount--
				}
			} else if !mt.metadataWaitLogged {
				logging.Debugf("download waiting for metadata hash=%s", hash)
				mt.metadataWaitLogged = true
			}
		}
	}

	if !diskFull {
		// Start queued torrents up to max limit
		for hash, mt := range e.managedTorrents {
			if activeCount >= e.cfg.MaxActiveDownloads {
				break
			}
			if mt.state == models.StateQueued {
				e.setStateLocked(hash, mt, models.StateDownloading, models.ErrNone, "resource slot available")
				if mt.t.Info() != nil {
					mt.t.DownloadAll() // start downloading all files (rarest-first by default)
					mt.downloadAllStarted = true
					logging.Debugf("download all started hash=%s files=%d", hash, len(mt.t.Files()))
				} else if !mt.metadataWaitLogged {
					logging.Debugf("torrent promoted to downloading while metadata is pending hash=%s", hash)
					mt.metadataWaitLogged = true
				}
				activeCount++
			}
		}
	}

	e.logResourceSummaryLocked(activeCount, diskFull, freeSpace, err)
}

func (e *Engine) logResourceSummaryLocked(activeCount int, diskFull bool, freeSpace uint64, freeSpaceErr error) {
	if !logging.IsDebugEnabled() {
		return
	}

	freeValue := "unknown"
	if freeSpaceErr == nil {
		freeValue = fmt.Sprintf("%d", freeSpace)
	}
	key := fmt.Sprintf("torrents=%d active=%d max=%d disk_full=%t free=%s", len(e.managedTorrents), activeCount, e.cfg.MaxActiveDownloads, diskFull, freeValue)
	now := time.Now()
	if key == e.lastResourceKey && !e.lastResourceLog.IsZero() && now.Sub(e.lastResourceLog) < 30*time.Second {
		return
	}

	logging.Debugf("resource manager %s", key)
	e.lastResourceKey = key
	e.lastResourceLog = now
}
