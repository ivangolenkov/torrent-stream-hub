package engine

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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
	client  *torrent.Client
	storage storage.ClientImplCloser
	cfg     *config.Config

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
	boostUntil                time.Time
	normalMaxEstablishedConns int
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

	eng := &Engine{
		cfg:             cfg,
		managedTorrents: make(map[string]*ManagedTorrent),
	}
	client, torrentStorage, err := eng.newTorrentClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create torrent client: %w", err)
	}
	eng.client = client
	eng.storage = torrentStorage
	eng.streamManager = NewStreamManager(eng)

	go eng.resourceMonitor()
	if cfg.BTSwarmWatchdogEnabled {
		go eng.swarmRefreshMonitor()
	}

	return eng, nil
}

func (e *Engine) newTorrentClient() (*torrent.Client, storage.ClientImplCloser, error) {
	pieceCompletion, completionBackend, completionPersistent, completionErr := e.newPieceCompletion()
	torrentStorage := storage.NewFileWithCompletion(e.cfg.DownloadDir, pieceCompletion)
	e.mu.Lock()
	e.pieceCompletionBackend = completionBackend
	e.pieceCompletionPersistent = completionPersistent
	e.pieceCompletionErr = completionErr
	e.mu.Unlock()
	clientConfig := buildClientConfig(e.cfg)
	clientConfig.DefaultStorage = torrentStorage
	clientConfig.Callbacks.StatusUpdated = append(clientConfig.Callbacks.StatusUpdated, func(event torrent.StatusUpdatedEvent) {
		e.handleStatusEvent(event)
	})
	clientConfig.Callbacks.PeerConnAdded = append(clientConfig.Callbacks.PeerConnAdded, func(conn *torrent.PeerConn) {
		e.logPeerConnEvent("added", conn)
	})
	clientConfig.Callbacks.PeerConnClosed = func(conn *torrent.PeerConn) {
		e.logPeerConnEvent("closed", conn)
	}
	client, err := torrent.NewClient(clientConfig)
	if err != nil {
		if closeErr := torrentStorage.Close(); closeErr != nil {
			logging.Warnf("failed to close torrent storage after client init error: %v", closeErr)
		}
		return nil, nil, err
	}
	return client, torrentStorage, nil
}

func (e *Engine) newPieceCompletion() (storage.PieceCompletion, string, bool, string) {
	pc, err := storage.NewBoltPieceCompletion(e.cfg.DownloadDir)
	if err == nil {
		return pc, "bolt", true, ""
	}
	msg := sanitizeDiagnosticError(err)
	logging.Warnf("persistent piece completion unavailable backend=bolt dir=%s: %s", e.cfg.DownloadDir, msg)
	return storage.NewMapPieceCompletion(), "memory", false, msg
}

func buildClientConfig(cfg *config.Config) *torrent.ClientConfig {
	config.ApplyDefaults(cfg)
	clientConfig := torrent.NewDefaultClientConfig()
	clientConfig.DataDir = cfg.DownloadDir
	clientConfig.ListenPort = cfg.TorrentPort
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
	clientConfig.UpnpID = "Torrent-Stream-Hub"

	// Use aggressive dial timeouts to quickly cycle through unresponsive/NAT-blocked peers
	// and free up half-open slots for potentially good peers.
	clientConfig.NominalDialTimeout = 5 * time.Second
	clientConfig.MinDialTimeout = 2 * time.Second
	clientConfig.HandshakesTimeout = 20 * time.Second // Give TCP peers more time to send the initial handshake

	clientConfig.PieceHashersPerTorrent = 4 // Increase hasher workers to prevent CPU bottleneck on fast connections

	applyPublicIPConfig(clientConfig, cfg)

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

func applyPublicIPConfig(clientConfig *torrent.ClientConfig, cfg *config.Config) {
	cfg.BTPublicIPv4Status = config.PublicIPStatus(cfg.BTPublicIPv4, false)
	cfg.BTPublicIPv6Status = config.PublicIPStatus(cfg.BTPublicIPv6, true)
	if status := config.PublicIPStatus(cfg.BTPublicIPv4, false); status == "configured" {
		clientConfig.PublicIp4 = net.ParseIP(strings.TrimSpace(cfg.BTPublicIPv4)).To4()
	}
	if status := config.PublicIPStatus(cfg.BTPublicIPv6, true); status == "configured" {
		clientConfig.PublicIp6 = net.ParseIP(strings.TrimSpace(cfg.BTPublicIPv6)).To16()
	}
	if !cfg.BTPublicIPDiscoveryEnabled {
		return
	}
	if clientConfig.PublicIp4 == nil {
		if ip := discoverPublicIP("https://api.ipify.org", false); ip != nil {
			clientConfig.PublicIp4 = ip
			cfg.BTPublicIPv4Status = "discovered"
		} else if cfg.BTPublicIPv4Status == "disabled" {
			cfg.BTPublicIPv4Status = "failed"
		}
	}
	if clientConfig.PublicIp6 == nil && !cfg.BTDisableIPv6 {
		if ip := discoverPublicIP("https://api64.ipify.org", true); ip != nil {
			clientConfig.PublicIp6 = ip
			cfg.BTPublicIPv6Status = "discovered"
		} else if cfg.BTPublicIPv6Status == "disabled" {
			cfg.BTPublicIPv6Status = "failed"
		}
	}
}

func discoverPublicIP(endpoint string, ipv6 bool) net.IP {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(endpoint)
	if err != nil {
		logging.Debugf("public ip discovery failed endpoint=%s: %v", logging.SafeURLSummary(endpoint), err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logging.Debugf("public ip discovery failed endpoint=%s status=%d", logging.SafeURLSummary(endpoint), resp.StatusCode)
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return nil
	}
	ip := net.ParseIP(strings.TrimSpace(string(body)))
	if ip == nil || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return nil
	}
	if ipv6 {
		if ip.To4() != nil || ip.To16() == nil {
			return nil
		}
		return ip.To16()
	}
	return ip.To4()
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
	logging.Infof("closing torrent engine")
	e.mu.Lock()
	client := e.client
	torrentStorage := e.storage
	e.client = nil
	e.storage = nil
	e.mu.Unlock()
	closeTorrentResources(client, torrentStorage)
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
			t.DownloadAll()
			mt.downloadAllStarted = true
			logging.Debugf("download all restarted after data recheck hash=%s files=%d", hash, len(t.Files()))
		}
	}()
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

func (e *Engine) BTHealth() *models.BTHealth {
	e.mu.RLock()
	mts := make([]*ManagedTorrent, 0, len(e.managedTorrents))
	for _, mt := range e.managedTorrents {
		mts = append(mts, mt)
	}
	now := time.Now()

	health := &models.BTHealth{
		SeedEnabled:                e.cfg.BTSeed,
		UploadEnabled:              !e.cfg.BTNoUpload,
		DHTEnabled:                 !e.cfg.BTDisableDHT,
		PEXEnabled:                 !e.cfg.BTDisablePEX,
		UPNPEnabled:                !e.cfg.BTDisableUPNP,
		TCPEnabled:                 !e.cfg.BTDisableTCP,
		UTPEnabled:                 !e.cfg.BTDisableUTP,
		IPv6Enabled:                !e.cfg.BTDisableIPv6,
		ListenPort:                 e.cfg.TorrentPort,
		ClientProfile:              normalizedBTClientProfile(e.cfg.BTClientProfile),
		DownloadProfile:            e.cfg.BTDownloadProfile,
		BenchmarkMode:              e.cfg.BTBenchmarkMode,
		EstablishedConnsPerTorrent: e.cfg.BTEstablishedConns,
		HalfOpenConnsPerTorrent:    e.cfg.BTHalfOpenConns,
		TotalHalfOpenConns:         e.cfg.BTTotalHalfOpen,
		PeersLowWater:              e.cfg.BTPeersLowWater,
		PeersHighWater:             e.cfg.BTPeersHighWater,
		DialRateLimit:              e.cfg.BTDialRateLimit,
		PublicIPDiscoveryEnabled:   e.cfg.BTPublicIPDiscoveryEnabled,
		PublicIPv4Status:           firstNonEmptyString(e.cfg.BTPublicIPv4Status, config.PublicIPStatus(e.cfg.BTPublicIPv4, false)),
		PublicIPv6Status:           firstNonEmptyString(e.cfg.BTPublicIPv6Status, config.PublicIPStatus(e.cfg.BTPublicIPv6, true)),
		RetrackersMode:             normalizedRetrackersMode(e.cfg.BTRetrackersMode),
		DownloadLimit:              e.cfg.DownloadLimit,
		UploadLimit:                e.cfg.UploadLimit,
		SwarmWatchdogEnabled:       e.cfg.BTSwarmWatchdogEnabled,
		SwarmCheckIntervalSec:      e.cfg.BTSwarmCheckIntervalSec,
		SwarmRefreshCooldownSec:    e.cfg.BTSwarmRefreshCooldownSec,
		InvalidMetainfoCount:       e.invalidMetainfoCount,
		PieceCompletionBackend:     e.pieceCompletionBackend,
		PieceCompletionPersistent:  e.pieceCompletionPersistent,
		PieceCompletionError:       e.pieceCompletionErr,
		PeerDropRatio:              e.cfg.BTSwarmPeerDropRatio,
		SeedDropRatio:              e.cfg.BTSwarmSeedDropRatio,
		SpeedDropRatio:             e.cfg.BTSwarmSpeedDropRatio,
		IncomingConnectivityNote:   "Incoming peers may not reach this client unless TCP/UDP torrent port is forwarded or UPnP succeeds.",
		Torrents:                   make([]models.BTTorrentHealth, 0, len(mts)),
	}
	e.mu.RUnlock()

	for _, mt := range mts {
		stats := mt.t.Stats()

		mt.mu.Lock()
		e.updateSpeedsLocked(mt, stats)
		maxEstablishedConns := mt.normalMaxEstablishedConns
		if maxEstablishedConns == 0 {
			maxEstablishedConns = e.cfg.BTEstablishedConns
		}
		if !mt.boostUntil.IsZero() && now.Before(mt.boostUntil) {
			maxEstablishedConns = e.cfg.BTSwarmBoostConns
		}
		mtTrackerStatus := mt.trackerStatus
		mtTrackerError := mt.trackerError
		mtDegraded := mt.degraded
		mtLastSwarmRefreshAt := mt.lastSwarmRefreshAt
		mtLastSwarmRefreshReason := mt.lastSwarmRefreshReason
		mtLastPeerRefreshAt := mt.lastPeerRefreshAt
		mtLastPeerRefreshReason := mt.lastPeerRefreshReason
		mtLastHealthyAt := mt.lastHealthyAt
		mtBoostUntil := mt.boostUntil
		mtDownloadSpeed := mt.downloadSpeed
		mtUploadSpeed := mt.uploadSpeed
		mtState := mt.state
		mt.mu.Unlock()

		info := mt.t.Info()
		trackerTiers, trackerURLs := trackerCounts(mt)
		activeStreams := e.streamManager.ActiveStreamsForTorrent(mt.t.InfoHash().HexString())

		health.Torrents = append(health.Torrents, models.BTTorrentHealth{
			Hash:                  mt.t.InfoHash().HexString(),
			Name:                  mt.t.Name(),
			State:                 mtState,
			Known:                 stats.TotalPeers,
			Connected:             stats.ActivePeers,
			Pending:               stats.PendingPeers,
			HalfOpen:              stats.HalfOpenPeers,
			Seeds:                 stats.ConnectedSeeders,
			BytesRead:             stats.BytesRead.Int64(),
			BytesReadData:         stats.BytesReadData.Int64(),
			BytesReadUsefulData:   stats.BytesReadUsefulData.Int64(),
			BytesWritten:          stats.BytesWritten.Int64(),
			BytesWrittenData:      stats.BytesWrittenData.Int64(),
			ChunksRead:            stats.ChunksRead.Int64(),
			ChunksReadUseful:      stats.ChunksReadUseful.Int64(),
			ChunksReadWasted:      stats.ChunksReadWasted.Int64(),
			PiecesDirtiedGood:     stats.PiecesDirtiedGood.Int64(),
			PiecesDirtiedBad:      stats.PiecesDirtiedBad.Int64(),
			DownloadSpeed:         mtDownloadSpeed,
			UploadSpeed:           mtUploadSpeed,
			WasteRatio:            wasteRatio(stats.ChunksReadUseful.Int64(), stats.ChunksReadWasted.Int64()),
			TrackerTiersCount:     trackerTiers,
			TrackerURLsCount:      trackerURLs,
			MetadataReady:         info != nil,
			TrackerStatus:         mtTrackerStatus,
			TrackerError:          mtTrackerError,
			Degraded:              mtDegraded,
			LastRefreshAt:         formatBTHealthTime(mtLastSwarmRefreshAt),
			LastRefreshReason:     mtLastSwarmRefreshReason,
			LastPeerRefreshAt:     formatBTHealthTime(mtLastPeerRefreshAt),
			LastPeerRefreshReason: mtLastPeerRefreshReason,
			LastHealthyAt:         formatBTHealthTime(mtLastHealthyAt),
			BoostedUntil:          formatBTHealthTime(mtBoostUntil),
			MaxEstablishedConns:   maxEstablishedConns,
			ActiveStreams:         activeStreams,
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

func sanitizeDiagnosticError(err error) string {
	if err == nil {
		return ""
	}
	return truncateDiagnostic(logging.SanitizeText(err.Error()))
}

func truncateDiagnostic(text string) string {
	const max = 1000
	if len(text) <= max {
		return text
	}
	return text[:max] + "..."
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

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func wasteRatio(useful, wasted int64) float64 {
	total := useful + wasted
	if total <= 0 {
		return 0
	}
	return float64(wasted) / float64(total)
}

func trackerCounts(mt *ManagedTorrent) (int, int) {
	if mt == nil || mt.t == nil {
		return 0, 0
	}
	mi := mt.t.Metainfo()
	list := mi.UpvertedAnnounceList()
	urls := 0
	for _, tier := range list {
		urls += len(tier)
	}
	return len(list), urls
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

	mt.mu.Lock()
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
	mt.mu.Unlock()

	if info != nil {
		model.PieceLength = info.PieceLength
		model.NumPieces = info.NumPieces()
		for i, file := range t.Files() {
			model.Files = append(model.Files, &models.File{
				Index:      i,
				Path:       file.DisplayPath(),
				Size:       file.Length(),
				Offset:     file.Offset(),
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

func (mt *ManagedTorrent) setStateLocked(state models.TorrentState, errReason models.ErrorReason, reason string) {
	oldState := mt.state
	oldErr := mt.err
	mt.state = state
	mt.err = errReason
	if oldState != state || oldErr != errReason {
		logging.Infof("torrent state change hash=%s state=%s->%s error=%s reason=%s", mt.t.InfoHash().HexString(), oldState, state, errReason, reason)
	}
}

func (e *Engine) watchMetadata(hash string, t *torrent.Torrent) {
	go func() {
		<-t.GotInfo()

		e.mu.RLock()
		mt, ok := e.managedTorrents[hash]
		e.mu.RUnlock()

		if !ok || mt.t != t || mt.metadataLogged {
			return
		}

		info := t.Info()
		if info == nil {
			return
		}

		fileCount := len(t.Files())
		mi := t.Metainfo()

		mt.mu.Lock()
		mt.metainfo = &mi
		mt.metadataLogged = true
		mt.metadataWaitLogged = false
		mt.mu.Unlock()

		if err := e.saveMetainfo(hash, &mi, true); err != nil {
			logging.Warnf("failed to persist runtime metainfo hash=%s: %v", hash, err)
		}

		logging.Infof("torrent metadata ready hash=%s name=%q size=%d files=%d", hash, t.Name(), info.TotalLength(), fileCount)

		mt.mu.Lock()
		state := mt.state
		downloadAllStarted := mt.downloadAllStarted
		if state == models.StateDownloading && !downloadAllStarted {
			mt.downloadAllStarted = true
		}
		mt.mu.Unlock()

		if state == models.StateDownloading && !downloadAllStarted {
			t.DownloadAll()
			logging.Debugf("download all started after metadata hash=%s files=%d", hash, fileCount)
		}
	}()
}

func (e *Engine) updateSpeedsLocked(mt *ManagedTorrent, stats torrent.TorrentStats) {
	now := time.Now()
	rawReadBytes := stats.BytesRead.Int64()
	dataReadBytes := stats.BytesReadData.Int64()
	readBytes := stats.BytesReadUsefulData.Int64()
	writtenBytes := stats.PeerConns.BytesWrittenData.Int64() + stats.WebSeeds.BytesWrittenData.Int64()

	if !mt.lastStatsAt.IsZero() {
		elapsed := now.Sub(mt.lastStatsAt).Seconds()
		if elapsed > 0 {
			rawReadDelta := rawReadBytes - mt.lastRawReadBytes
			dataReadDelta := dataReadBytes - mt.lastDataReadBytes
			readDelta := readBytes - mt.lastReadBytes
			writtenDelta := writtenBytes - mt.lastWrittenBytes
			if rawReadDelta < 0 {
				rawReadDelta = 0
			}
			if dataReadDelta < 0 {
				dataReadDelta = 0
			}
			if readDelta < 0 {
				readDelta = 0
			}
			if writtenDelta < 0 {
				writtenDelta = 0
			}
			mt.rawDownloadSpeed = int64(float64(rawReadDelta) / elapsed)
			mt.dataDownloadSpeed = int64(float64(dataReadDelta) / elapsed)
			mt.downloadSpeed = int64(float64(readDelta) / elapsed)
			mt.uploadSpeed = int64(float64(writtenDelta) / elapsed)
		}
	}

	mt.lastStatsAt = now
	mt.lastRawReadBytes = rawReadBytes
	mt.lastDataReadBytes = dataReadBytes
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
	if e.client == nil {
		return "disabled"
	}
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

	e.mu.RLock()
	mt, ok := e.managedTorrents[hash]
	e.mu.RUnlock()

	if !ok {
		return
	}

	mt.mu.Lock()
	defer mt.mu.Unlock()

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

	e.manageResources()

	for range ticker.C {
		e.manageResources()
	}
}

type swarmSnapshot struct {
	State             models.TorrentState
	MetadataReady     bool
	Complete          bool
	AddedAt           time.Time
	ActiveStreams     int
	Known             int
	Connected         int
	Pending           int
	HalfOpen          int
	Seeds             int
	DownloadSpeed     int64
	PeakConnected     int
	PeakSeeds         int
	PeakDownloadSpeed int64
	PeakUpdatedAt     time.Time
	Now               time.Time
}

type swarmDecision struct {
	Degraded     bool
	NeedsRefresh bool
	Reason       string
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

	e.mu.RLock()
	torrents := make([]*ManagedTorrent, 0, len(e.managedTorrents))
	for _, mt := range e.managedTorrents {
		torrents = append(torrents, mt)
	}
	e.mu.RUnlock()

	for _, mt := range torrents {
		stats := mt.t.Stats()

		mt.mu.Lock()
		e.updateSpeedsLocked(mt, stats)

		info := mt.t.Info()
		complete := info != nil && mt.t.BytesCompleted() == info.TotalLength()

		if mt.state == models.StateDownloading && !complete && mt.downloadSpeed == 0 {
			if mt.stallStartedAt.IsZero() {
				mt.stallStartedAt = now
			} else if now.Sub(mt.stallStartedAt) > time.Minute {
				// Soft refresh (re-announce to DHT/Trackers)
				if mt.lastSwarmRefreshAt.IsZero() || now.Sub(mt.lastSwarmRefreshAt) > 3*time.Minute {
					mt.lastSwarmRefreshAt = now
					mt.lastSwarmRefreshReason = "stalled"
					hash := mt.t.InfoHash().HexString()
					go e.refreshPeerDiscovery(hash, mt.t, "stalled", info != nil)
				}
			}
		} else {
			mt.stallStartedAt = time.Time{}
		}
		mt.mu.Unlock()
	}
}

func (e *Engine) saveMetainfo(hash string, mi *metainfo.MetaInfo, keepExisting bool) error {
	if mi == nil || strings.TrimSpace(hash) == "" {
		return nil
	}
	dir := filepath.Join(e.ConfigDir(), "metainfo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, strings.ToLower(strings.TrimSpace(hash))+".torrent")
	if keepExisting {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	writeErr := mi.Write(f)
	closeErr := f.Close()
	if writeErr != nil {
		_ = os.Remove(tmp)
		return writeErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	return os.Rename(tmp, path)
}

func (e *Engine) refreshDHTAsync(hash string, t *torrent.Torrent, reason string) {
	if e.client == nil {
		return
	}
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

func (e *Engine) refreshPeerDiscovery(hash string, t *torrent.Torrent, reason string, downloadAll bool) {
	if t == nil {
		return
	}
	t.SetMaxEstablishedConns(e.cfg.BTSwarmBoostConns)
	t.AllowDataDownload()
	if !e.cfg.BTNoUpload {
		t.AllowDataUpload()
	}
	if downloadAll {
		t.DownloadAll()
	}
	trackers := e.retrackers()
	if len(trackers) > 0 && !strings.EqualFold(strings.TrimSpace(e.cfg.BTRetrackersMode), "off") {
		t.AddTrackers([][]string{trackers})
	}
	e.refreshDHTAsync(hash, t, reason)
	logging.Infof("peer discovery refreshed hash=%s reason=%s boost_conns=%d trackers=%d download_all=%t", hash, reason, e.cfg.BTSwarmBoostConns, len(trackers), downloadAll)
}

func (e *Engine) manageResources() {
	minFreeBytes := uint64(e.cfg.MinFreeSpaceGB) * 1024 * 1024 * 1024

	freeSpace, err := GetFreeSpace(e.cfg.DownloadDir)
	if err != nil {
		logging.Warnf("failed to check free disk space dir=%s: %v", e.cfg.DownloadDir, err)
	}
	diskFull := err == nil && freeSpace < minFreeBytes

	e.mu.RLock()
	torrents := make([]*ManagedTorrent, 0, len(e.managedTorrents))
	for _, mt := range e.managedTorrents {
		torrents = append(torrents, mt)
	}
	e.mu.RUnlock()

	activeCount := 0

	for _, mt := range torrents {
		if diskFull {
			mt.mu.Lock()
			if mt.state == models.StateDownloading {
				mt.setStateLocked(models.StateDiskFull, models.ErrDiskFull, "free disk space below limit")
				mt.downloadAllStarted = false
				mt.mu.Unlock()
				if mt.t.Info() != nil {
					for _, f := range mt.t.Files() {
						f.SetPriority(torrent.PiecePriorityNone)
					}
				}
			} else {
				mt.mu.Unlock()
			}
			continue
		}

		mt.mu.Lock()
		if mt.state == models.StateDiskFull {
			mt.setStateLocked(models.StateQueued, models.ErrNone, "free disk space recovered")
		}

		if mt.state == models.StateDownloading {
			activeCount++
			if info := mt.t.Info(); info != nil {
				if !mt.downloadAllStarted {
					mt.downloadAllStarted = true
					mt.mu.Unlock()
					mt.t.DownloadAll()
					logging.Debugf("download all started hash=%s files=%d", mt.t.InfoHash().HexString(), len(mt.t.Files()))
					mt.mu.Lock()
				}
				if mt.t.BytesCompleted() == info.TotalLength() {
					mt.setStateLocked(models.StateSeeding, models.ErrNone, "download complete")
					activeCount--
				}
			} else if !mt.metadataWaitLogged {
				logging.Debugf("download waiting for metadata hash=%s", mt.t.InfoHash().HexString())
				mt.metadataWaitLogged = true
			}
		}
		mt.mu.Unlock()
	}

	if !diskFull {
		for _, mt := range torrents {
			mt.mu.Lock()
			if activeCount >= e.cfg.MaxActiveDownloads {
				mt.mu.Unlock()
				break
			}
			if mt.state == models.StateQueued {
				mt.setStateLocked(models.StateDownloading, models.ErrNone, "resource slot available")
				if mt.t.Info() != nil {
					mt.downloadAllStarted = true
					mt.mu.Unlock()
					mt.t.DownloadAll()
					logging.Debugf("download all started hash=%s files=%d", mt.t.InfoHash().HexString(), len(mt.t.Files()))
					mt.mu.Lock()
				} else if !mt.metadataWaitLogged {
					logging.Debugf("torrent promoted to downloading while metadata is pending hash=%s", mt.t.InfoHash().HexString())
					mt.metadataWaitLogged = true
				}
				activeCount++
			}
			mt.mu.Unlock()
		}
	}

	e.logResourceSummary(activeCount, diskFull, freeSpace, err)
}

func (e *Engine) logResourceSummary(activeCount int, diskFull bool, freeSpace uint64, freeSpaceErr error) {
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
