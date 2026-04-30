package engine

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/models"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/time/rate"
)

type Engine struct {
	client *torrent.Client
	cfg    *config.Config

	mu              sync.RWMutex
	managedTorrents map[string]*ManagedTorrent

	streamManager *StreamManager
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
		return nil, fmt.Errorf("torrent not found: %s", hash)
	}

	files := mt.t.Files()
	if index < 0 || index >= len(files) {
		return nil, fmt.Errorf("file index out of bounds")
	}

	return files[index], nil
}

func (e *Engine) Close() {
	e.client.Close()
}

func (e *Engine) AddMagnet(magnet string) (*models.Torrent, error) {
	t, err := e.client.AddMagnet(magnet)
	if err != nil {
		return nil, err
	}

	return e.addTorrent(t)
}

func (e *Engine) AddTorrentFile(r io.Reader) (*models.Torrent, error) {
	metaInfo, err := metainfo.Load(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read torrent file: %w", err)
	}

	t, err := e.client.AddTorrent(metaInfo)
	if err != nil {
		return nil, err
	}

	return e.addTorrent(t)
}

func (e *Engine) addTorrent(t *torrent.Torrent) (*models.Torrent, error) {
	hash := t.InfoHash().HexString()

	e.mu.Lock()
	e.managedTorrents[hash] = &ManagedTorrent{
		t:     t,
		state: models.StateQueued, // initially queued, resourceMonitor will start it
		err:   models.ErrNone,
	}
	e.mu.Unlock()

	return e.mapTorrent(t, models.StateQueued, models.ErrNone), nil
}

func (e *Engine) Pause(hash string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	mt, ok := e.managedTorrents[hash]
	if !ok {
		return fmt.Errorf("torrent not found: %s", hash)
	}

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

func (e *Engine) GetAllTorrents() []*models.Torrent {
	e.mu.RLock()
	defer e.mu.RUnlock()

	torrents := make([]*models.Torrent, 0, len(e.managedTorrents))
	for _, mt := range e.managedTorrents {
		torrents = append(torrents, e.mapTorrent(mt.t, mt.state, mt.err))
	}
	return torrents
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

	model := &models.Torrent{
		Hash:       t.InfoHash().HexString(),
		Name:       t.Name(),
		Size:       size,
		Downloaded: downloaded,
		Progress:   progress,
		State:      state,
		Error:      errReason,
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
				if info := mt.t.Info(); info != nil && mt.t.BytesCompleted() == info.TotalLength() {
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
