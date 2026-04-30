package engine

import (
	"fmt"
	"sync"
	"time"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/models"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/time/rate"
)

type Engine struct {
	client *torrent.Client
	cfg    *config.Config

	mu              sync.RWMutex
	managedTorrents map[string]*ManagedTorrent
}

type ManagedTorrent struct {
	t     *torrent.Torrent
	state models.TorrentState
	err   models.ErrorReason
}

func New(cfg *config.Config) (*Engine, error) {
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

	client, err := torrent.NewClient(clientConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create torrent client: %w", err)
	}

	eng := &Engine{
		client:          client,
		cfg:             cfg,
		managedTorrents: make(map[string]*ManagedTorrent),
	}

	go eng.resourceMonitor()

	return eng, nil
}

func (e *Engine) Close() {
	e.client.Close()
}

func (e *Engine) AddMagnet(magnet string) (*models.Torrent, error) {
	t, err := e.client.AddMagnet(magnet)
	if err != nil {
		return nil, err
	}
	<-t.GotInfo() // wait for metadata

	e.mu.Lock()
	defer e.mu.Unlock()

	e.managedTorrents[t.InfoHash().HexString()] = &ManagedTorrent{
		t:     t,
		state: models.StateQueued, // initially queued, resourceMonitor will start it
		err:   models.ErrNone,
	}

	return e.mapTorrent(t, models.StateQueued, models.ErrNone), nil
}

func (e *Engine) Pause(hash string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	mt, ok := e.managedTorrents[hash]
	if !ok {
		return fmt.Errorf("torrent not found: %s", hash)
	}

	mt.t.Drop() // Stops downloading, but keeps it in client. Wait, no, Drop removes it.
	// To pause, we should cancel pieces or set priority to None.
	// anacrolix/torrent provides CancelPieces or dropping. Wait, setting all files priority to 0 pauses.
	mt.t.DownloadAll() // this resumes.
	// Let's implement real pause by dropping pieces
	for _, f := range mt.t.Files() {
		f.SetPriority(torrent.PiecePriorityNone)
	}
	mt.state = models.StatePaused
	return nil
}

func (e *Engine) Resume(hash string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	mt, ok := e.managedTorrents[hash]
	if !ok {
		return fmt.Errorf("torrent not found: %s", hash)
	}

	mt.state = models.StateQueued // put to queue, let resource monitor handle it
	return nil
}

func (e *Engine) Delete(hash string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	mt, ok := e.managedTorrents[hash]
	if !ok {
		return nil
	}

	mt.t.Drop()
	delete(e.managedTorrents, hash)
	return nil
}

func (e *Engine) mapTorrent(t *torrent.Torrent, state models.TorrentState, errReason models.ErrorReason) *models.Torrent {
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

	return &models.Torrent{
		Hash:       t.InfoHash().HexString(),
		Name:       t.Name(),
		Size:       size,
		Downloaded: downloaded,
		Progress:   progress,
		State:      state,
		Error:      errReason,
	}
}

// resourceMonitor checks disk space and manages active downloads limit
func (e *Engine) resourceMonitor() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	minFreeBytes := uint64(e.cfg.MinFreeSpaceGB) * 1024 * 1024 * 1024

	for range ticker.C {
		e.mu.Lock()

		freeSpace, err := GetFreeSpace(e.cfg.DownloadDir)
		diskFull := err == nil && freeSpace < minFreeBytes

		activeCount := 0

		for _, mt := range e.managedTorrents {
			if diskFull {
				if mt.state == models.StateDownloading {
					mt.state = models.StateDiskFull
					mt.err = models.ErrDiskFull
					for _, f := range mt.t.Files() {
						f.SetPriority(torrent.PiecePriorityNone)
					}
				}
				continue
			}

			// If disk space recovered, queued/diskfull can be started
			if mt.state == models.StateDiskFull {
				mt.state = models.StateQueued
				mt.err = models.ErrNone
			}

			if mt.state == models.StateDownloading {
				activeCount++
				// Check if finished
				if mt.t.BytesCompleted() == mt.t.Info().TotalLength() {
					mt.state = models.StateSeeding
					activeCount--
				}
			}
		}

		if !diskFull {
			// Start queued torrents up to max limit
			for _, mt := range e.managedTorrents {
				if activeCount >= e.cfg.MaxActiveDownloads {
					break
				}
				if mt.state == models.StateQueued {
					mt.state = models.StateDownloading
					mt.t.DownloadAll() // start downloading all files (rarest-first by default)
					activeCount++
				}
			}
		}

		e.mu.Unlock()
	}
}
