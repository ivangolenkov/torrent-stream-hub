package usecase

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"github.com/anacrolix/torrent/metainfo"
	"os"
	"path/filepath"
	"time"

	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"
)

var ErrInventoryPending = errors.New("storage inventory pending")

func (uc *TorrentUseCase) wakeInventory() {
	select {
	case uc.inventoryWake <- struct{}{}:
	default:
	}
}

func (uc *TorrentUseCase) initializeInventory() {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		uc.inventoryInitError = err
		return
	}
	uc.inventorySession = hex.EncodeToString(token[:])
	volumes, err := uc.repo.StorageVolumes()
	if err != nil {
		uc.inventoryInitError = err
		return
	}
	uc.volumes = volumes
	if len(volumes) == 0 {
		if err := uc.enrollVolume(".", true); err != nil && !errors.Is(err, payload.ErrVolumeUnregistered) {
			uc.inventoryInitError = err
			return
		}
	}
	uc.inventoryInitError = uc.repo.BeginInventorySession(uc.inventorySession)
}

func (uc *TorrentUseCase) enrollVolume(path string, fresh bool) error {
	root, err := os.OpenRoot(uc.engine.DownloadDir())
	if err != nil {
		return err
	}
	defer root.Close()
	volumeRoot, err := payload.FindVolumeRoot(root, path)
	if err != nil {
		return err
	}
	v, err := uc.repo.BeginVolumeEnrollment(volumeRoot, fresh)
	if err != nil {
		return err
	}
	if v.State == "enrolling" {
		if err := payload.EnrollVolume(root, *v); err != nil {
			return err
		}
		if err := uc.repo.FinishVolumeEnrollment(*v); err != nil {
			return err
		}
	}
	volumes, err := uc.repo.StorageVolumes()
	if err != nil {
		return err
	}
	uc.volumeMu.Lock()
	uc.volumes = volumes
	uc.volumeMu.Unlock()
	return payload.CheckVolume(root, path, volumes)
}

func (uc *TorrentUseCase) checkVolume(path string) error {
	if uc.inventoryInitError != nil {
		return uc.inventoryInitError
	}
	root, err := os.OpenRoot(uc.engine.DownloadDir())
	if err != nil {
		return errors.Join(payload.ErrVolumeUnavailable, err)
	}
	defer root.Close()
	uc.volumeMu.RLock()
	volumes := append([]payload.VolumeRegistration(nil), uc.volumes...)
	uc.volumeMu.RUnlock()
	return payload.CheckVolume(root, path, volumes)
}

func (uc *TorrentUseCase) inventoryAdmitted(m *repository.StorageMapping) error {
	if uc.inventoryInitError != nil {
		return uc.inventoryInitError
	}
	i, err := uc.repo.Inventory(m.StorageID)
	if err != nil {
		return err
	}
	if i == nil || i.Session != uc.inventorySession || i.Revision != m.Revision || (i.Status != "ready" && i.Status != "observing") {
		return ErrStorageConflict
	}
	return nil
}

// At most 128 stats per visit, never a full disk traversal or a hash pass.
// Missing files without persisted expectation are ordinary incomplete payload.
func (uc *TorrentUseCase) inspectInventory(ctx context.Context, hash string, repair bool, supplied ...*metainfo.Info) (bool, error) {
	ticket, err := uc.coordinator.tryAcquire("inventory/" + hash)
	if err != nil {
		return false, nil
	}
	defer ticket.release()
	t, err := uc.repo.InventoryRecord(ctx, hash)
	if err != nil || t == nil {
		return false, err
	}
	m, err := uc.repo.StorageMapping(hash)
	if err != nil {
		return false, err
	}
	if m != nil && m.State == "reserved" && m.Root == "" {
		return true, uc.checkVolume(".")
	}
	if !m.Ready() || t.DeletionMode != "" {
		return false, nil
	}
	recovery, err := uc.repo.Recovery(hash)
	if err != nil {
		return false, err
	}
	if recoveryBlocks(recovery) && recovery.Reason != models.StorageDeletionCancelled {
		return false, nil
	}
	state, err := uc.repo.Inventory(m.StorageID)
	if err != nil {
		return false, err
	}
	if state != nil && state.Session == uc.inventorySession && state.Revision == m.Revision && state.Status == "ready" && !repair {
		return true, nil
	}
	if state != nil && state.Status == "blocked" && !repair {
		return false, nil
	}
	cursor, reason := -1, ""
	if state != nil && (state.Status == "pending" || state.Status == "observing") && state.Revision == m.Revision && state.Session == uc.inventorySession {
		cursor, reason = state.Cursor, state.Reason
	}
	if err := uc.checkVolume(m.Root); err != nil {
		reason = "volume_unavailable"
		if errors.Is(err, payload.ErrVolumeUnregistered) {
			reason = "volume_unregistered"
		}
		return false, uc.repo.SaveInventory(m, t.Control.RecordID, uc.inventorySession, "blocked", reason, cursor, nil, false)
	}
	files, err := uc.repo.InventoryFiles(m, t.Control.RecordID, cursor)
	if err != nil {
		return false, err
	}
	var info *metainfo.Info
	if len(supplied) > 0 {
		info = supplied[0]
	} else {
		info, err = uc.metadataStorageInfo(t)
	}
	if err != nil {
		if !t.MetadataKnown {
			if _, e := os.Stat(filepath.Join(uc.engine.ConfigDir(), "metainfo", hash+".torrent")); errors.Is(e, os.ErrNotExist) {
				return false, ErrMetadataPending
			}
		}
		return false, uc.repo.SaveInventory(m, t.Control.RecordID, uc.inventorySession, "blocked", "metadata_error", cursor, nil, false)
	}
	if info == nil && len(files) > 0 {
		return false, ErrMetadataPending
	}
	var metadataFiles []metainfo.FileInfo
	if info != nil {
		metadataFiles = info.UpvertedFiles()
	}
	root, err := os.OpenRoot(uc.engine.DownloadDir())
	if err != nil {
		return false, err
	}
	defer root.Close()
	for index := range files {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		f := &files[index]
		f.ObservedAt = time.Now().UnixMilli()
		if f.Index < 0 || f.Index >= len(metadataFiles) {
			return false, ErrStorageConflict
		}
		path, err := m.Layout().File(info, &metadataFiles[f.Index])
		if err != nil {
			return false, err
		}
		f.MetadataPath = payload.Path(info, &metadataFiles[f.Index])
		opened, err := payload.Open(root, path)
		switch {
		case err == nil:
			f.Presence, f.Expected = "present", true
			if err := opened.Close(); err != nil {
				return false, err
			}
		case errors.Is(err, os.ErrNotExist):
			f.Presence, f.Reason = "missing", "not_created"
			if f.Expected && !repair {
				f.Reason = "expected_file_missing"
				reason = f.Reason
			}
			if repair {
				f.Expected = false
			}
		default:
			f.Presence, f.Reason = "unknown", "access_error"
			reason = "access_error"
		}
		cursor = f.Index
	}
	status := "pending"
	if state != nil && state.Status == "observing" {
		status = "observing"
	}
	if len(files) < 128 {
		status = "ready"
		if reason != "" {
			status = "blocked"
		}
	}
	err = uc.repo.SaveInventory(m, t.Control.RecordID, uc.inventorySession, status, reason, cursor, files, repair)
	return status == "ready", err
}

func (uc *TorrentUseCase) runInventory(ctx context.Context) {
	// Bootstrap performs one bounded visit per record first. Starting the
	// round-robin worker afterwards avoids competing with that same visit.
	select {
	case <-ctx.Done():
		return
	case <-uc.inventoryWake:
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	cursor := ""
	nextObservation := time.Now().Add(30 * time.Second)
	for {
		periodic := time.Now().After(nextObservation)
		hashes, err := uc.repo.PendingInventories(ctx, cursor, periodic)
		if err == nil {
			for _, hash := range hashes {
				if ctx.Err() != nil {
					return
				}
				cursor = hash
				if periodic {
					m, e := uc.repo.StorageMapping(hash)
					t, e2 := uc.repo.GetTorrent(hash)
					if e == nil && e2 == nil && m.Ready() && t != nil {
						i, _ := uc.repo.Inventory(m.StorageID)
						if i != nil && i.Status == "ready" {
							_ = uc.repo.SaveInventory(m, t.Control.RecordID, uc.inventorySession, "observing", "", -1, nil, false)
						}
					}
				}
				ready, e := uc.inspectInventory(ctx, hash, false)
				if e == nil && ready {
					_, _ = uc.restorePersistedTorrent(ctx, hash)
				}
				if e == nil && !ready {
					uc.detachInventoryBlocked(ctx, hash)
				}
			}
			if len(hashes) < 64 {
				cursor = ""
				if periodic {
					nextObservation = time.Now().Add(30 * time.Second)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-uc.inventoryWake:
		}
	}
}

func (uc *TorrentUseCase) reconcileInventory(ctx context.Context, m *repository.StorageMapping, id, path string, deleting, workflowBlocked bool) error {
	i, err := uc.repo.GetIntent(m.Hash)
	if err != nil {
		return err
	}
	if i == nil || i.Version.RecordID != id {
		return ErrStorageConflict
	}
	if !deleting {
		if _, err := uc.repo.CommitRunIntent(i.Version, models.RunPaused); err != nil {
			return err
		}
	}
	if err := uc.engine.Delete(m.Hash); err != nil {
		return err
	}
	if path == "" {
		path = m.Root
	}
	if path == "" {
		path = "."
	}
	if err := uc.enrollVolume(path, false); err != nil {
		return err
	}
	if workflowBlocked {
		return nil
	}
	if err := uc.repo.QueueInventory(m, id, uc.inventorySession); err != nil {
		return err
	}
	select {
	case uc.inventoryWake <- struct{}{}:
	default:
	}
	return nil
}

// Existing explicit Resume authorizes repair of expected missing files. Volume
// registration, foreign blocks and verification remain independent vetoes.
func (uc *TorrentUseCase) repairMissingInventory(hash string) error {
	m, err := uc.repo.StorageMapping(hash)
	if err != nil || m == nil {
		return err
	}
	i, err := uc.repo.Inventory(m.StorageID)
	if err != nil || i == nil {
		return err
	}
	if i.Reason != "expected_file_missing" {
		return nil
	}
	if err := uc.checkVolume(m.Root); err != nil {
		return err
	}
	t, err := uc.repo.GetTorrent(hash)
	if err != nil {
		return err
	}
	if t == nil {
		return ErrTorrentNotFound
	}
	if err := uc.repo.QueueInventory(m, t.Control.RecordID, uc.inventorySession); err != nil {
		return err
	}
	for {
		ready, err := uc.inspectInventory(uc.engine.Context(), hash, true)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		i, err := uc.repo.Inventory(m.StorageID)
		if err != nil {
			return err
		}
		if i == nil || i.Status == "blocked" {
			return ErrStorageConflict
		}
		select {
		case <-uc.engine.Context().Done():
			return context.Canceled
		case <-time.After(time.Millisecond):
		}
	}
}

func (uc *TorrentUseCase) isVolumeMarker(path string) bool {
	uc.volumeMu.RLock()
	for _, v := range uc.volumes {
		if filepath.Clean(path) == filepath.Join(v.Root, payload.VolumeMarker) {
			uc.volumeMu.RUnlock()
			return true
		}
	}
	uc.volumeMu.RUnlock()
	return false
}

func (uc *TorrentUseCase) guardPayloadIO(path string) error {
	if uc.isVolumeMarker(path) {
		return payload.ErrUnsafePath
	}
	return uc.checkVolume(path)
}

func (uc *TorrentUseCase) detachInventoryBlocked(ctx context.Context, hash string) {
	release, err := uc.tryLockRecord(hash)
	if err != nil {
		return
	}
	defer release()
	m, err := uc.repo.StorageMapping(hash)
	if err != nil || m == nil {
		return
	}
	state, err := uc.repo.Inventory(m.StorageID)
	if err == nil && state != nil && state.Status == "blocked" {
		_ = uc.engine.Delete(hash)
	}
}

// This permits network metadata only. The library resolver still refuses to
// open payload until supplied metadata has passed the inventory worker.
func (uc *TorrentUseCase) canFetchOnlyMetadata(t *models.Torrent) bool {
	if t.MetadataKnown {
		return false
	}
	if _, err := os.Stat(filepath.Join(uc.engine.ConfigDir(), "metainfo", t.Hash+".torrent")); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	m, err := uc.repo.StorageMapping(t.Hash)
	if err != nil || m == nil {
		return false
	}
	if uc.checkStorageWorkflow(t.Hash) != nil {
		return false
	}
	path := m.Root
	if path == "" {
		path = "."
	}
	return uc.checkVolume(path) == nil
}
