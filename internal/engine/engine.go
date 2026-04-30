package engine

import (
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

	mu              sync.RWMutex
	managedTorrents map[string]*ManagedTorrent
	lastResourceLog time.Time
	lastResourceKey string

	streamManager *StreamManager
}

type ManagedTorrent struct {
	t                  *torrent.Torrent
	state              models.TorrentState
	err                models.ErrorReason
	metadataLogged     bool
	metadataWaitLogged bool
	downloadAllStarted bool
	lastStatsAt        time.Time
	lastReadBytes      int64
	lastWrittenBytes   int64
	downloadSpeed      int64
	uploadSpeed        int64
	lastPeerSummary    models.PeerSummary
	lastPeerLog        time.Time
	trackerStatus      string
	trackerError       string
}

func New(cfg *config.Config) (*Engine, error) {
	logging.Infof("initializing torrent engine download_dir=%s torrent_port=%d max_downloads=%d min_free_space_gb=%d", cfg.DownloadDir, cfg.TorrentPort, cfg.MaxActiveDownloads, cfg.MinFreeSpaceGB)

	clientConfig := torrent.NewDefaultClientConfig()
	clientConfig.DataDir = cfg.DownloadDir
	clientConfig.ListenPort = cfg.TorrentPort

	// Limit mmap: Use standard file storage instead of mmap which consumes too much RAM
	clientConfig.DefaultStorage = storage.NewFile(cfg.DownloadDir)

	if cfg.DownloadLimit > 0 {
		clientConfig.DownloadRateLimiter = rate.NewLimiter(rate.Limit(cfg.DownloadLimit), cfg.DownloadLimit)
	}
	if cfg.UploadLimit > 0 {
		clientConfig.UploadRateLimiter = rate.NewLimiter(rate.Limit(cfg.UploadLimit), cfg.UploadLimit)
	}

	var eng *Engine
	clientConfig.Callbacks.StatusUpdated = append(clientConfig.Callbacks.StatusUpdated, func(event torrent.StatusUpdatedEvent) {
		if eng != nil {
			eng.handleStatusEvent(event)
		}
	})
	clientConfig.Callbacks.PeerConnAdded = append(clientConfig.Callbacks.PeerConnAdded, func(conn *torrent.PeerConn) {
		if eng != nil {
			eng.logPeerConnEvent("added", conn)
		}
	})
	clientConfig.Callbacks.PeerConnClosed = func(conn *torrent.PeerConn) {
		if eng != nil {
			eng.logPeerConnEvent("closed", conn)
		}
	}

	client, err := torrent.NewClient(clientConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create torrent client: %w", err)
	}

	eng = &Engine{
		client:          client,
		cfg:             cfg,
		managedTorrents: make(map[string]*ManagedTorrent),
	}
	eng.streamManager = NewStreamManager(eng)

	go eng.resourceMonitor()

	return eng, nil
}

func (e *Engine) StreamManager() *StreamManager {
	return e.streamManager
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

func (e *Engine) Close() {
	logging.Infof("closing torrent engine")
	e.client.Close()
}

func (e *Engine) AddMagnet(magnet string) (*models.Torrent, error) {
	logging.Infof("adding torrent from magnet %s", logging.SafeMagnetSummary(magnet))
	t, err := e.client.AddMagnet(magnet)
	if err != nil {
		logging.Warnf("failed to add magnet %s: %v", logging.SafeMagnetSummary(magnet), err)
		return nil, err
	}

	model, err := e.addTorrent(t)
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

	t, err := e.client.AddTorrent(metaInfo)
	if err != nil {
		logging.Warnf("failed to add .torrent hash=%s: %v", metaInfo.HashInfoBytes().HexString(), err)
		return nil, err
	}

	model, err := e.addTorrent(t)
	if err != nil {
		return nil, err
	}
	model.SourceURI = metaInfo.Magnet(nil, nil).String()
	logging.Infof("torrent added hash=%s source=file state=%s", model.Hash, model.State)
	return model, nil
}

func (e *Engine) addTorrent(t *torrent.Torrent) (*models.Torrent, error) {
	hash := t.InfoHash().HexString()

	e.mu.Lock()
	if mt, ok := e.managedTorrents[hash]; ok {
		model := e.mapManagedTorrent(mt)
		e.mu.Unlock()
		logging.Debugf("torrent already managed hash=%s state=%s", hash, model.State)
		return model, nil
	}

	mt := &ManagedTorrent{
		t:     t,
		state: models.StateQueued, // initially queued, resourceMonitor will start it
		err:   models.ErrNone,
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
