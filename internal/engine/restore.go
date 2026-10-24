package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"torrent-stream-hub/internal/models"
)

// StageExistingRestore closes admission before a Resume transaction and reloads
// authoritative settings rather than trusting an earlier runtime snapshot.
func (e *Engine) StageExistingRestore(saved *models.Torrent) error {
	e.admissionMu.Lock()
	defer e.admissionMu.Unlock()
	if e.ctx.Err() != nil {
		return e.ctx.Err()
	}
	e.mu.RLock()
	mt := e.managedTorrents[saved.Hash]
	e.mu.RUnlock()
	if mt == nil {
		return TorrentNotFoundError{Hash: saved.Hash}
	}
	mt.mu.Lock()
	checking := mt.rechecking || mt.verificationUncertain
	mt.mu.Unlock()
	if checking {
		return ErrOperationConflict
	}
	priorities, err := persistedPriorities(saved, mt.t)
	if err != nil {
		return err
	}
	mt.mu.Lock()
	d, err := mt.applyStateEventLocked(stateEvent{kind: stateRestoreStaged})
	if err != nil {
		mt.mu.Unlock()
		return err
	}
	mt.filePriorities = priorities
	mt.queuePosition = saved.QueuePosition
	mt.mu.Unlock()
	e.applyQuiesceLocked(mt, d, "torrent initialization is not complete")
	return nil
}

func persistedPriorities(saved *models.Torrent, t *torrent.Torrent) (map[int]models.FilePriority, error) {
	priorities := make(map[int]models.FilePriority, len(saved.Files))
	for _, f := range saved.Files {
		if f == nil {
			continue
		}
		if f.Index < 0 || (t.Info() != nil && f.Index >= len(t.Files())) || (f.Priority != models.PriorityNormal && f.Priority != models.PriorityHigh && f.Priority != models.PriorityNone) {
			return nil, fmt.Errorf("invalid persisted file priority")
		}
		priorities[f.Index] = f.Priority
	}
	return priorities, nil
}

// RestoreTorrent stages persisted intent before the torrent becomes visible to
// the scheduler. The caller must persist any application changes before FinishRestore.
func (e *Engine) RestoreTorrent(spec *torrent.TorrentSpec, saved *models.Torrent) (*models.Torrent, error) {
	if saved == nil || spec == nil {
		return nil, ErrOperationConflict
	}
	if _, err := runtimeTransition(runtimePolicy{}, stateEvent{kind: stateRegistered, phase: saved.State, err: saved.Error, staged: true, diskReason: saved.DownloadBlock}); err != nil {
		return nil, err
	}
	if !e.beginWork() {
		return nil, context.Canceled
	}
	defer e.wg.Done()
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
	spec.DisallowDataUpload = true
	if source, err := torrent.TorrentSpecFromMagnetUri(saved.SourceURI); err == nil && source.PrivateSafe {
		spec.PrivateSafe = true
	}
	inactive := spec.DisableNetworking || saved.State == models.StatePaused || saved.State == models.StateError || saved.State == models.StateMissingFiles
	if inactive {
		spec.DisableNetworking = true
		spec.Trackers = nil
		spec.Webseeds = nil
		spec.DhtNodes = nil
		spec.PeerAddrs = nil
		spec.Sources = nil
	} else {
		e.augmentTorrentSpec(spec)
	}
	t, _, err := e.client.AddTorrentSpec(spec)
	if err != nil {
		return nil, err
	}
	return e.registerTorrent(t, saved.SourceURI, saved, saved.Hash)
}

func (e *Engine) FinishRestore(hash string, state models.TorrentState, manualResume ...bool) error {
	e.admissionMu.Lock()
	if e.ctx.Err() != nil {
		e.admissionMu.Unlock()
		return e.ctx.Err()
	}
	e.mu.RLock()
	mt := e.managedTorrents[hash]
	e.mu.RUnlock()
	if mt == nil {
		e.admissionMu.Unlock()
		return TorrentNotFoundError{Hash: hash}
	}
	mt.mu.Lock()
	event := stateEvent{kind: stateActivated, phase: state, err: mt.err, fullReconcile: true}
	_, policyErr := runtimeTransition(mt.policy, event)
	mt.mu.Unlock()
	if policyErr != nil {
		e.admissionMu.Unlock()
		return policyErr
	}
	var metadata *metainfo.MetaInfo
	if mt.t.Info() != nil {
		v := mt.t.Metainfo()
		metadata = &v
	}
	needsNetworkSpec := !mt.t.NetworkingEnabled()
	var networkSpec *torrent.TorrentSpec
	var metadataErr error
	if err := e.runRuntimeWorkLocked(mt, func() {
		if metadata != nil {
			metadataErr = e.saveMetainfo(hash, metadata, true)
		}
		if metadataErr == nil && needsNetworkSpec {
			if mi, err := metainfo.LoadFromFile(filepath.Join(e.ConfigDir(), "metainfo", hash+".torrent")); err == nil {
				networkSpec, _ = torrent.TorrentSpecFromMetaInfoErr(mi)
			}
		}
	}); err != nil {
		e.admissionMu.Unlock()
		return err
	}
	if metadataErr != nil {
		faultErr := e.quiesceFaultLocked(mt, stateMetadataFailed)
		e.admissionMu.Unlock()
		return errors.Join(fmt.Errorf("persist runtime metainfo: %w", metadataErr), faultErr)
	}
	mt.mu.Lock()
	for index, priority := range mt.filePriorities {
		if index < 0 || (mt.t.Info() != nil && index >= len(mt.t.Files())) || (priority != models.PriorityNormal && priority != models.PriorityHigh && priority != models.PriorityNone) {
			mt.mu.Unlock()
			faultErr := e.quiesceFaultLocked(mt, stateSelectionInvalid)
			e.admissionMu.Unlock()
			return errors.Join(fmt.Errorf("persisted file index out of bounds"), faultErr)
		}
	}
	if len(manualResume) > 0 && manualResume[0] {
		mt.mu.Unlock()
		writeVersion := mt.t.WriteChunkErrorVersion()
		probe, minimum := e.diskProbe, e.minimumDiskBytes()
		var probeErr error
		if err := e.runRuntimeWorkLocked(mt, func() { probeErr = e.diskErrorWithProbe(hash, probe, minimum) }); err != nil {
			e.admissionMu.Unlock()
			return err
		}
		if err := probeErr; err != nil {
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
		event.diskRecovered = true
	}
	d, err := mt.applyStateEventLocked(event)
	if err != nil {
		mt.mu.Unlock()
		e.admissionMu.Unlock()
		return err
	}
	if event.diskRecovered {
		mt.downloadBlockPersisted = false
	}
	inactive := d.next.state == models.StatePaused || d.next.state == models.StateError || d.next.state == models.StateMissingFiles || d.next.state == models.StateDiskFull
	if !inactive {
		mt.payloadCtx, mt.payloadCancel = context.WithCancelCause(e.ctx)
	}
	mt.mu.Unlock()
	if !inactive {
		if !mt.t.NetworkingEnabled() {
			if networkSpec != nil {
				e.augmentTorrentSpec(networkSpec)
				if err := mt.t.MergeSpec(networkSpec); err != nil {
					_ = e.quiesceFaultLocked(mt, stateApplyFailed)
					e.admissionMu.Unlock()
					return err
				}
			}
			mt.t.EnableNetworking()
		}
	}
	mt.t.DisallowDataDownload()
	if d.allowUpload && !e.cfg.BTNoUpload {
		mt.t.AllowDataUpload()
	}
	if d.closeUpload {
		mt.t.DisallowDataUpload()
	}
	if inactive && mt.t.Info() != nil {
		for _, file := range mt.t.Files() {
			file.SetPriority(torrent.PiecePriorityNone)
		}
		mt.t.CancelPieces(0, mt.t.NumPieces())
		if e.streamManager != nil {
			_ = e.streamManager.refreshTorrentOverlayLocked(mt)
		}
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
