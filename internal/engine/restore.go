package engine

import (
	"context"
	"fmt"

	"github.com/anacrolix/torrent"
	"torrent-stream-hub/internal/models"
)

// StageExistingRestore closes admission before a Resume transaction and reloads
// authoritative settings rather than trusting an earlier runtime snapshot.
func (e *Engine) StageExistingRestore(saved *models.Torrent) error {
	e.admissionMu.Lock()
	defer e.admissionMu.Unlock()
	e.mu.RLock()
	mt := e.managedTorrents[saved.Hash]
	e.mu.RUnlock()
	if mt == nil {
		return TorrentNotFoundError{Hash: saved.Hash}
	}
	mt.t.DisallowDataDownload()
	mt.mu.Lock()
	defer mt.mu.Unlock()
	mt.initializing = true
	if mt.payloadCancel != nil {
		mt.payloadCancel(DownloadBlockedError{"torrent initialization is not complete"})
	}
	mt.downloadAllStarted = false
	priorities := make(map[int]models.FilePriority)
	for _, f := range saved.Files {
		if f != nil {
			if f.Index < 0 || (f.Priority != models.PriorityNormal && f.Priority != models.PriorityHigh && f.Priority != models.PriorityNone) {
				return fmt.Errorf("invalid persisted file priority")
			}
			priorities[f.Index] = f.Priority
		}
	}
	mt.filePriorities = priorities
	return nil
}

// RestoreTorrent stages persisted intent before the torrent becomes visible to
// the scheduler. The caller must persist any application changes before FinishRestore.
func (e *Engine) RestoreTorrent(spec *torrent.TorrentSpec, saved *models.Torrent) (*models.Torrent, error) {
	if spec.InfoHash.HexString() != saved.Hash && (!spec.InfoHashV2.Ok || spec.InfoHashV2.Value.ToShort().HexString() != saved.Hash) {
		return nil, fmt.Errorf("restored source does not match persisted infohash")
	}
	for _, f := range saved.Files {
		if f != nil && (f.Index < 0 || (f.Priority != models.PriorityNormal && f.Priority != models.PriorityHigh && f.Priority != models.PriorityNone)) {
			return nil, fmt.Errorf("invalid persisted file priority")
		}
	}
	if e.GetRawTorrent(saved.Hash) != nil {
		if err := e.StageExistingRestore(saved); err != nil {
			return nil, err
		}
	}
	spec.DisallowDataDownload = true
	e.augmentTorrentSpec(spec)
	t, _, err := e.client.AddTorrentSpec(spec)
	if err != nil {
		return nil, err
	}
	return e.registerTorrent(t, saved.SourceURI, saved, saved.Hash)
}

func (e *Engine) FinishRestore(hash string, state models.TorrentState, manualResume ...bool) error {
	e.admissionMu.Lock()
	e.mu.RLock()
	mt := e.managedTorrents[hash]
	e.mu.RUnlock()
	if mt == nil {
		e.admissionMu.Unlock()
		return TorrentNotFoundError{Hash: hash}
	}
	if mt.t.Info() != nil {
		mi := mt.t.Metainfo()
		if err := e.saveMetainfo(hash, &mi, true); err != nil {
			e.admissionMu.Unlock()
			return fmt.Errorf("persist runtime metainfo: %w", err)
		}
	}
	mt.mu.Lock()
	for index, priority := range mt.filePriorities {
		if index < 0 || (mt.t.Info() != nil && index >= len(mt.t.Files())) || (priority != models.PriorityNormal && priority != models.PriorityHigh && priority != models.PriorityNone) {
			mt.mu.Unlock()
			e.admissionMu.Unlock()
			return fmt.Errorf("persisted file index out of bounds")
		}
	}
	if len(manualResume) > 0 && manualResume[0] {
		mt.mu.Unlock()
		writeVersion := mt.t.WriteChunkErrorVersion()
		if err := e.diskError(hash); err != nil {
			persistErr := e.blockDownloadLocked(mt, err.Error())
			e.admissionMu.Unlock()
			if persistErr != nil {
				return persistErr
			}
			return DownloadBlockedError{err.Error()}
		}
		if err := mt.t.ClearWriteChunkErrorIfVersion(writeVersion); err != nil {
			persistErr := e.blockDownloadLocked(mt, "payload write failed during Resume")
			e.admissionMu.Unlock()
			if persistErr != nil {
				return persistErr
			}
			return DownloadBlockedError{Reason: "payload write failed during Resume"}
		}
		if e.saveDownloadBlock != nil {
			if err := e.saveDownloadBlock(hash, ""); err != nil {
				e.admissionMu.Unlock()
				return err
			}
		}
		mt.mu.Lock()
		mt.downloadBlock = ""
		mt.downloadBlockPersisted = false
	}
	mt.initializing = false
	mt.state = state
	mt.downloadAllStarted = false
	inactive := state == models.StatePaused || state == models.StateError || state == models.StateMissingFiles || state == models.StateDiskFull || mt.downloadBlock != ""
	if !inactive {
		mt.state = models.StateQueued
		mt.err = models.ErrNone
		mt.payloadCtx, mt.payloadCancel = context.WithCancelCause(e.ctx)
	} else if mt.downloadBlock != "" && state != models.StatePaused {
		mt.state = models.StateDiskFull
		mt.err = models.ErrorReason(mt.downloadBlock)
	}
	diskBlocked := mt.downloadBlock != "" || state == models.StateDiskFull
	mt.mu.Unlock()
	mt.t.DisallowDataDownload()
	if state != models.StatePaused && (!inactive || diskBlocked) && !e.cfg.BTNoUpload {
		mt.t.AllowDataUpload()
	}
	if state == models.StatePaused || (inactive && !diskBlocked) {
		mt.t.DisallowDataUpload()
	}
	e.admissionMu.Unlock()
	if !inactive {
		e.manageResources()
	}
	if len(manualResume) > 0 && manualResume[0] {
		mt.mu.Lock()
		reason := mt.downloadBlock
		mt.mu.Unlock()
		if reason != "" {
			return DownloadBlockedError{Reason: reason}
		}
	}
	return nil
}
