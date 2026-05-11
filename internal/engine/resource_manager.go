package engine

import (
	"context"
	"fmt"
	"time"

	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"

	"github.com/anacrolix/torrent"
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
					e.applyFilePrioritiesAndDownload(mt)
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
					e.applyFilePrioritiesAndDownload(mt)
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
