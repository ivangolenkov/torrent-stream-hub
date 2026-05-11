package engine

import (
	"path/filepath"
	"strings"
	"time"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"

	"github.com/anacrolix/torrent"
)

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
		mtPeakDownloadSpeed := mt.peakDownloadSpeed
		mtPeakUpdatedAt := mt.peakUpdatedAt
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
			PeakDownloadSpeed:     mtPeakDownloadSpeed,
			PeakUpdatedAt:         formatBTHealthTime(mtPeakUpdatedAt),
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

		mt.mu.Lock()
		for i, file := range t.Files() {
			prio := models.PriorityNormal
			if p, ok := mt.filePriorities[i]; ok {
				prio = p
			}
			model.Files = append(model.Files, &models.File{
				Index:      i,
				Path:       file.DisplayPath(),
				Size:       file.Length(),
				Offset:     file.Offset(),
				Downloaded: file.BytesCompleted(),
				Priority:   prio,
				IsMedia:    isMediaFile(file.DisplayPath()),
			})
		}
		mt.mu.Unlock()
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
