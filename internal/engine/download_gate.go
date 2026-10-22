package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
)

var ErrDownloadBlocked = errors.New("payload download is blocked")

type DownloadBlockedError struct{ Reason string }

func (e DownloadBlockedError) Error() string        { return e.Reason }
func (e DownloadBlockedError) Is(target error) bool { return target == ErrDownloadBlocked }

// The callback must not acquire application lifecycle or library locks.
func (e *Engine) SetDownloadBlockPersistence(save func(string, string) error) {
	e.admissionMu.Lock()
	e.saveDownloadBlock = save
	e.admissionMu.Unlock()
}

func (e *Engine) diskError(hash string) error {
	root, err := e.StorageRoot(hash)
	if err != nil {
		return err
	}
	path := filepath.Join(e.cfg.DownloadDir, root)
	paths := []string{path}
	e.mu.RLock()
	mt := e.managedTorrents[hash]
	e.mu.RUnlock()
	if mt != nil && mt.t != nil {
		if info := mt.t.Info(); info != nil {
			for _, file := range info.UpvertedFiles() {
				paths = append(paths, filepath.Dir(filepath.Join(path, payload.Path(info, &file))))
			}
		}
	}
	seen := make(map[string]bool)
	for _, path := range paths {
		if err := e.checkDiskPath(path, seen); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) checkDiskPath(path string, seen map[string]bool) error {
	// A new root may not exist yet. Probe its nearest existing parent filesystem.
	for {
		if _, err := os.Stat(path); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("disk space unknown: %w", err)
		}
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	if seen[path] {
		return nil
	}
	seen[path] = true
	probe := e.diskProbe
	if probe == nil {
		probe = GetFreeSpace
	}
	free, err := probe(path)
	if err != nil {
		return fmt.Errorf("disk space unknown: %w", err)
	}
	if free < e.minimumDiskBytes() {
		return fmt.Errorf("disk free space below configured reserve")
	}
	return nil
}

func (e *Engine) minimumDiskBytes() uint64 {
	const gb = uint64(1024 * 1024 * 1024)
	minimum := uint64(max(e.cfg.MinFreeSpaceGB, 0))
	if minimum > ^uint64(0)/gb {
		return ^uint64(0)
	}
	return minimum * gb
}

// admissionMu is held; the prohibition takes effect before persistence.
func (e *Engine) blockDownloadLocked(mt *ManagedTorrent, reason string) error {
	mt.mu.Lock()
	changed := mt.downloadBlock != reason
	if changed {
		mt.downloadBlockPersisted = false
	}
	persist := !mt.downloadBlockPersisted
	mt.downloadBlock = reason
	if mt.payloadCancel != nil {
		mt.payloadCancel(DownloadBlockedError{reason})
	}
	mt.downloadAllStarted = false
	if mt.state != models.StatePaused {
		mt.setStateLocked(models.StateDiskFull, models.ErrorReason(reason), "payload gate closed")
	}
	mt.mu.Unlock()
	mt.t.DisallowDataDownload()
	if e.streamManager != nil {
		_ = e.streamManager.refreshTorrentOverlayLocked(mt)
	}
	if persist && e.saveDownloadBlock != nil {
		if err := e.saveDownloadBlock(mt.hash, reason); err != nil {
			logging.Warnf("failed to persist download block hash=%s: %v", mt.hash, err)
			return err
		}
		mt.mu.Lock()
		mt.downloadBlockPersisted = true
		mt.mu.Unlock()
	}
	return nil
}

func (e *Engine) payloadWriteError(mt *ManagedTorrent, err error) {
	e.admissionMu.Lock()
	defer e.admissionMu.Unlock()
	if e.ctx.Err() != nil {
		return
	}
	e.mu.RLock()
	current := e.managedTorrents[mt.hash]
	e.mu.RUnlock()
	if current != mt {
		return
	}
	// Async notifications may arrive after a successful explicit Resume. Read
	// the synchronous library latch under admission serialization, not stale err.
	if err == nil {
		err = mt.t.LastWriteChunkError()
		if err == nil {
			return
		}
	}
	reason := "payload storage write failed"
	if errors.Is(err, syscall.ENOSPC) {
		reason = "Disk full: ENOSPC from payload writer"
	}
	_ = e.blockDownloadLocked(mt, reason)
}

func (e *Engine) CheckPayloadAdmission(hash string) error {
	e.admissionMu.Lock()
	defer e.admissionMu.Unlock()
	e.mu.RLock()
	mt := e.managedTorrents[hash]
	e.mu.RUnlock()
	if mt == nil {
		return TorrentNotFoundError{Hash: hash}
	}
	return e.admitPayloadLocked(mt)
}

// All engine Allow paths are serialized here. File/reader priorities alone never
// grant admission. Callers must hold admissionMu and must not hold mt.mu.
func (e *Engine) admitPayloadLocked(mt *ManagedTorrent) error {
	if e.ctx != nil && e.ctx.Err() != nil {
		return e.ctx.Err()
	}
	mt.mu.Lock()
	blocked, initializing, state := mt.downloadBlock, mt.initializing || mt.rechecking || mt.verificationUncertain, mt.state
	mt.mu.Unlock()
	if blocked != "" {
		return DownloadBlockedError{blocked}
	}
	if initializing {
		return DownloadBlockedError{"torrent initialization is not complete"}
	}
	if state == models.StatePaused || state == models.StateError || state == models.StateMissingFiles || state == models.StateDiskFull {
		return DownloadBlockedError{"torrent is not accepting payload downloads"}
	}
	if err := e.diskError(mt.hash); err != nil {
		if persistErr := e.blockDownloadLocked(mt, err.Error()); persistErr != nil {
			return persistErr
		}
		return DownloadBlockedError{err.Error()}
	}
	if mt.t.Info() != nil {
		mi := mt.t.Metainfo()
		if err := e.saveMetainfo(mt.hash, &mi, true); err != nil {
			mt.t.DisallowDataDownload()
			mt.mu.Lock()
			mt.initializing = true
			mt.setStateLocked(models.StateError, models.ErrorReason("metadata persistence failed"), "metadata initialization failed")
			mt.mu.Unlock()
			return err
		}
	}
	if err := e.applyFilePrioritiesAndDownload(mt); err != nil {
		return err
	}
	if err := mt.t.AllowDataDownloadIfNoWriteError(); err != nil {
		reason := "payload storage write failed"
		if errors.Is(err, syscall.ENOSPC) {
			reason = "Disk full: ENOSPC from payload writer"
		}
		if persistErr := e.blockDownloadLocked(mt, reason); persistErr != nil {
			return persistErr
		}
		return DownloadBlockedError{reason}
	}
	return nil
}
