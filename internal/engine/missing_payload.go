package engine

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/anacrolix/torrent"
	"torrent-stream-hub/internal/payload"
)

// Called only for a file with current-open verified readiness observed before
// the storage/read check. Ordinary incomplete files, missing metadata and hash
// mismatch are not evidence of physical disappearance. No background scan.
func (e *Engine) detectMissingExpectedPayload(mt *ManagedTorrent, file *torrent.File) error {
	rootName, err := e.StorageRoot(mt.hash)
	if err != nil {
		return err
	}
	fi := file.FileInfo()
	rel := filepath.Join(rootName, payload.Path(mt.t.Info(), &fi))
	if !isSafeRelativePath(rel) {
		return nil
	}
	e.admissionMu.Lock()
	defer e.admissionMu.Unlock()
	if !e.currentTorrent(mt) {
		return ErrOperationConflict
	}
	mt.mu.Lock()
	veto := mt.initializing || mt.rechecking || mt.verificationUncertain
	mt.mu.Unlock()
	if veto {
		return ErrOperationConflict
	}
	missing := false
	if err := e.runRuntimeWorkLocked(mt, func() {
		root, err := os.OpenRoot(e.cfg.DownloadDir)
		if err != nil {
			missing = errors.Is(err, fs.ErrNotExist)
			return
		}
		defer root.Close()
		if _, err = root.Lstat(rel); !errors.Is(err, fs.ErrNotExist) {
			return
		}
		_, err = root.Lstat(rel + ".part")
		missing = errors.Is(err, fs.ErrNotExist)
	}); err != nil {
		return err
	}
	if !missing {
		return nil
	}
	if err := e.quiesceFaultLocked(mt, statePayloadMissing); err != nil {
		return err
	}
	return fs.ErrNotExist
}

// Current-open proof survives readiness invalidation: the storage library can
// revoke completion asynchronously before the next read notices disappearance.
// Explicit Resume discards this expectation when authorizing network repair.
func (mt *ManagedTorrent) rememberExpectedPayload(index int, ready bool) bool {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	if ready && !mt.terminal {
		if mt.expectedPayload == nil {
			mt.expectedPayload = make(map[int]bool)
		}
		mt.expectedPayload[index] = true
	}
	return mt.expectedPayload[index]
}
