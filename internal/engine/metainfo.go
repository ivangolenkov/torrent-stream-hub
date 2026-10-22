package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

func (e *Engine) newPieceCompletion() (storage.PieceCompletion, string, bool, string) {
	pc, err := storage.NewBoltPieceCompletion(e.cfg.DownloadDir)
	if err == nil {
		return pc, "bolt", true, ""
	}
	msg := sanitizeDiagnosticError(err)
	logging.Warnf("persistent piece completion unavailable backend=bolt dir=%s: %s", e.cfg.DownloadDir, msg)
	return storage.NewMapPieceCompletion(), "memory", false, msg
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
	f, err := os.CreateTemp(dir, strings.ToLower(strings.TrimSpace(hash))+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	writeErr := mi.Write(f)
	if writeErr == nil {
		writeErr = f.Sync()
	}
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

func (e *Engine) watchMetadata(hash string, t *torrent.Torrent) {
	e.RunStorageTask(func(ctx context.Context) {
		select {
		case <-ctx.Done():
			return
		case <-t.Closed():
			return
		case <-t.GotInfo():
		}
		e.admissionMu.Lock()
		defer e.admissionMu.Unlock()

		e.mu.RLock()
		mt, ok := e.managedTorrents[hash]
		e.mu.RUnlock()

		if !ok || mt.t != t || ctx.Err() != nil {
			return
		}
		e.statusTorrents.Store(t.InfoHash().HexString(), mt)

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
			t.DisallowDataDownload()
			mt.mu.Lock()
			mt.initializing = true
			mt.setStateLocked(models.StateError, models.ErrorReason("metadata persistence failed"), "metadata initialization failed")
			mt.mu.Unlock()
			return
		}

		logging.Infof("torrent metadata ready hash=%s name=%q size=%d files=%d", hash, t.Name(), info.TotalLength(), fileCount)

		mt.mu.Lock()
		state := mt.state
		downloadAllStarted := mt.downloadAllStarted
		initializing := mt.initializing || mt.downloadBlock != ""
		if state == models.StateDownloading && !downloadAllStarted && !initializing {
			mt.downloadAllStarted = true
		}
		mt.mu.Unlock()

		if state == models.StateDownloading && !downloadAllStarted && !initializing {
			_ = e.admitPayloadLocked(mt)
			logging.Debugf("download all started after metadata hash=%s files=%d", hash, fileCount)
		}
	})
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
