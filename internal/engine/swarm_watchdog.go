package engine

import (
	"context"
	"strings"
	"time"

	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"

	"github.com/anacrolix/torrent"
)

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

func (e *Engine) swarmRefreshMonitor(ctx context.Context) {
	interval := time.Duration(e.cfg.BTSwarmCheckIntervalSec) * time.Second
	if interval <= 0 {
		interval = time.Minute
	}
	logging.Infof("swarm refresh monitor started interval=%s", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logging.Debugf("swarm refresh monitor stopped")
			return
		case <-ticker.C:
			e.checkSwarms()
		}
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
		var refreshTorrent *torrent.Torrent
		var refreshReason string
		refreshDownloadAll := false

		mt.mu.Lock()
		e.updateSpeedsLocked(mt, stats)

		info := mt.t.Info()
		complete := info != nil && mt.t.BytesCompleted() == info.TotalLength()
		if !complete {
			e.updatePeakDownloadSpeedLocked(mt, now)
		}
		lowSpeedReason := e.lowSpeedRefreshReasonLocked(mt, info != nil, complete, now)

		if lowSpeedReason != "" {
			if mt.stallStartedAt.IsZero() {
				mt.stallStartedAt = now
			} else if now.Sub(mt.stallStartedAt) > e.swarmStalledDuration() {
				// Soft refresh (re-announce to DHT/Trackers)
				if mt.lastSwarmRefreshAt.IsZero() || now.Sub(mt.lastSwarmRefreshAt) > e.swarmRefreshCooldown() {
					mt.lastSwarmRefreshAt = now
					mt.lastSwarmRefreshReason = lowSpeedReason
					refreshTorrent = mt.t
					refreshReason = lowSpeedReason
					refreshDownloadAll = info != nil
				}
			}
		} else {
			mt.stallStartedAt = time.Time{}
		}
		mt.mu.Unlock()

		if refreshTorrent != nil {
			go e.refreshPeerDiscovery(mt, refreshReason, refreshDownloadAll)
		}
	}
}

func (e *Engine) updatePeakDownloadSpeedLocked(mt *ManagedTorrent, now time.Time) {
	peakTTL := time.Duration(e.cfg.BTSwarmPeakTTLSec) * time.Second
	if peakTTL <= 0 {
		peakTTL = 30 * time.Minute
	}

	if !mt.peakUpdatedAt.IsZero() && now.Sub(mt.peakUpdatedAt) > peakTTL {
		mt.peakDownloadSpeed = 0
		mt.peakUpdatedAt = time.Time{}
	}

	if mt.state != models.StateDownloading || mt.downloadSpeed <= 0 {
		return
	}

	if mt.downloadSpeed > mt.peakDownloadSpeed {
		mt.peakDownloadSpeed = mt.downloadSpeed
		mt.peakUpdatedAt = now
	}
}

func (e *Engine) lowSpeedRefreshReasonLocked(mt *ManagedTorrent, metadataReady bool, complete bool, now time.Time) string {
	if mt.state != models.StateDownloading || !metadataReady || complete {
		return ""
	}

	stalledSpeed := int64(e.cfg.BTSwarmStalledSpeedBps)
	if stalledSpeed <= 0 {
		stalledSpeed = 32768
	}
	if mt.downloadSpeed < stalledSpeed {
		return "stalled_speed_below_threshold"
	}

	peakTTL := time.Duration(e.cfg.BTSwarmPeakTTLSec) * time.Second
	if peakTTL <= 0 {
		peakTTL = 30 * time.Minute
	}
	if mt.peakDownloadSpeed <= 0 || mt.peakUpdatedAt.IsZero() || now.Sub(mt.peakUpdatedAt) > peakTTL {
		return ""
	}

	ratio := e.cfg.BTSwarmSpeedDropRatio
	if ratio <= 0 || ratio >= 1 {
		ratio = 0.35
	}
	if mt.downloadSpeed < int64(float64(mt.peakDownloadSpeed)*ratio) {
		return "speed_dropped_below_peak"
	}

	return ""
}

func (e *Engine) swarmStalledDuration() time.Duration {
	d := time.Duration(e.cfg.BTSwarmStalledDurationSec) * time.Second
	if d <= 0 {
		return 3 * time.Minute
	}
	return d
}

func (e *Engine) swarmRefreshCooldown() time.Duration {
	d := time.Duration(e.cfg.BTSwarmRefreshCooldownSec) * time.Second
	if d <= 0 {
		return 3 * time.Minute
	}
	return d
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

func (e *Engine) refreshPeerDiscovery(mt *ManagedTorrent, reason string, downloadAll bool) {
	if mt == nil || mt.t == nil {
		return
	}
	t := mt.t
	hash := t.InfoHash().HexString()
	t.SetMaxEstablishedConns(e.cfg.BTSwarmBoostConns)
	t.AllowDataDownload()
	if !e.cfg.BTNoUpload {
		t.AllowDataUpload()
	}
	if downloadAll {
		e.applyFilePrioritiesAndDownload(mt)
	}
	trackers := e.retrackers()
	if len(trackers) > 0 && !strings.EqualFold(strings.TrimSpace(e.cfg.BTRetrackersMode), "off") {
		t.AddTrackers([][]string{trackers})
	}
	e.refreshDHTAsync(hash, t, reason)
	logging.Infof("peer discovery refreshed hash=%s reason=%s boost_conns=%d trackers=%d download_all=%t", hash, reason, e.cfg.BTSwarmBoostConns, len(trackers), downloadAll)
}
