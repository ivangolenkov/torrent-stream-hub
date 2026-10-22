package engine

import (
	"context"
	"fmt"
	"time"

	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"
)

// resourceMonitor checks disk space and manages active downloads limit.
func (e *Engine) resourceMonitor(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	e.manageResources()

	for {
		select {
		case <-ctx.Done():
			logging.Debugf("resource monitor stopped")
			return
		case <-ticker.C:
			e.manageResources()
		}
	}
}

func (e *Engine) manageResources() {
	e.admissionMu.Lock()
	defer e.admissionMu.Unlock()
	minFreeBytes := e.minimumDiskBytes()

	freeSpace, err := GetFreeSpace(e.cfg.DownloadDir)
	if err != nil {
		logging.Warnf("failed to check free disk space dir=%s: %v", e.cfg.DownloadDir, err)
	}
	diskFull := err != nil || freeSpace < minFreeBytes

	e.mu.RLock()
	torrents := make([]*ManagedTorrent, 0, len(e.managedTorrents))
	for _, mt := range e.managedTorrents {
		torrents = append(torrents, mt)
	}
	totalTorrents := len(e.managedTorrents)
	e.mu.RUnlock()

	activeCount := 0

	for _, mt := range torrents {
		if mt == nil || mt.t == nil {
			continue
		}
		mt.t.RefreshStorageCompletion()
		mt.mu.Lock()
		initializing := mt.initializing
		mt.mu.Unlock()
		if initializing {
			continue
		}
		if probeErr := e.diskError(mt.hash); probeErr != nil {
			_ = e.blockDownloadLocked(mt, probeErr.Error())
			continue
		}

		mt.mu.Lock()
		if mt.downloadBlock != "" || mt.state == models.StateDiskFull {
			reason := mt.downloadBlock
			mt.mu.Unlock()
			if reason != "" {
				_ = e.blockDownloadLocked(mt, reason)
			}
			continue
		}

		if mt.state == models.StateDownloading {
			activeCount++
			if info := mt.t.Info(); info != nil {
				if !mt.downloadAllStarted {
					mt.downloadAllStarted = true
					mt.mu.Unlock()
					_ = e.admitPayloadLocked(mt)
					logging.Debugf("download all started hash=%s files=%d", mt.t.InfoHash().HexString(), len(mt.t.Files()))
					mt.mu.Lock()
				}
				if mt.state == models.StateDownloading && mt.t.BytesCompleted() == info.TotalLength() {
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

	for _, mt := range torrents {
		if mt == nil || mt.t == nil {
			continue
		}
		mt.mu.Lock()
		if activeCount >= e.cfg.MaxActiveDownloads {
			mt.mu.Unlock()
			break
		}
		if mt.state == models.StateQueued && !mt.initializing && mt.downloadBlock == "" {
			mt.setStateLocked(models.StateDownloading, models.ErrNone, "resource slot available")
			if mt.t.Info() != nil {
				mt.downloadAllStarted = true
				mt.mu.Unlock()
				_ = e.admitPayloadLocked(mt)
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

	e.logResourceSummary(totalTorrents, activeCount, diskFull, freeSpace, err)
}

func (e *Engine) logResourceSummary(totalTorrents, activeCount int, diskFull bool, freeSpace uint64, freeSpaceErr error) {
	if !logging.IsDebugEnabled() {
		return
	}

	freeValue := "unknown"
	if freeSpaceErr == nil {
		freeValue = fmt.Sprintf("%d", freeSpace)
	}
	key := fmt.Sprintf("torrents=%d active=%d max=%d disk_full=%t free=%s", totalTorrents, activeCount, e.cfg.MaxActiveDownloads, diskFull, freeValue)
	now := time.Now()

	e.resourceLogMu.Lock()
	defer e.resourceLogMu.Unlock()
	if key == e.lastResourceKey && !e.lastResourceLog.IsZero() && now.Sub(e.lastResourceLog) < 30*time.Second {
		return
	}

	logging.Debugf("resource manager %s", key)
	e.lastResourceKey = key
	e.lastResourceLog = now
}
