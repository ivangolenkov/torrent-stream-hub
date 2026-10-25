package usecase

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
)

type storageObservation struct {
	recordID string
	version  int64
	reason   models.StorageRecoveryReason
	progress models.StorageProgress
}

type migrationFailure struct {
	recordID, first string
	count           int64
	loggedAt        time.Time
	kind            models.StorageRecoveryReason
}

func (uc *TorrentUseCase) observeMigrationFailure(t *models.Torrent, cause error) bool {
	uc.storageLiveMu.Lock()
	defer uc.storageLiveMu.Unlock()
	if uc.migrationFailures == nil {
		uc.migrationFailures = make(map[string]migrationFailure)
	}
	if cause == nil {
		delete(uc.migrationFailures, t.Hash)
		return false
	}
	v := uc.migrationFailures[t.Hash]
	now := time.Now()
	log := false
	if v.recordID != t.Control.RecordID || v.first != cause.Error() {
		v = migrationFailure{recordID: t.Control.RecordID, first: cause.Error(), count: 1, loggedAt: now, kind: models.StorageOwnershipBlocked}
		if errors.Is(cause, ErrOwnershipCatalogChanged) {
			v.kind = models.StorageCatalogChanged
		} else if errors.Is(cause, engine.ErrMigrationDiskFull) {
			v.kind = models.StorageInsufficientSpace
		}
		log = true
	} else {
		v.count++
		if now.Sub(v.loggedAt) >= time.Minute {
			v.loggedAt = now
			log = true
		}
	}
	uc.migrationFailures[t.Hash] = v
	return log
}

func int64ptr(v int64) *int64 { return &v }

// A single bounded latest-value slot per active operation. Heartbeat is sampled
// independently from the I/O worker, never advances last-progress, and never
// writes a checkpoint or probes storage.
func (uc *TorrentUseCase) startStorageProgress(hash, id string, version int64, reason models.StorageRecoveryReason) func() {
	uc.storageLiveMu.Lock()
	if uc.storageLive == nil {
		uc.storageLive = make(map[string]*storageObservation)
	}
	uc.storageLive[hash] = &storageObservation{recordID: id, version: version, reason: reason}
	uc.storageLiveMu.Unlock()
	uc.storageStage(hash, "preflight", nil, nil)
	return func() {
		uc.storageLiveMu.Lock()
		if v := uc.storageLive[hash]; v != nil && v.recordID == id && v.version == version {
			if uc.storageStageHook != nil {
				uc.storageStageHook(hash, v.progress, time.Duration(time.Now().UnixMilli()-v.progress.StageStartedAt)*time.Millisecond)
			}
			delete(uc.storageLive, hash)
		}
		uc.storageLiveMu.Unlock()
	}
}
func (uc *TorrentUseCase) storageStage(hash, stage string, total, files *int64) {
	now := time.Now().UnixMilli()
	uc.storageLiveMu.Lock()
	defer uc.storageLiveMu.Unlock()
	if v := uc.storageLive[hash]; v != nil {
		if uc.storageStageHook != nil && v.progress.Stage != "" {
			uc.storageStageHook(hash, v.progress, time.Duration(now-v.progress.StageStartedAt)*time.Millisecond)
		}
		v.progress = models.StorageProgress{Stage: stage, BytesTotal: total, FilesTotal: files, StageStartedAt: now, LastProgressAt: now, HeartbeatAt: now}
	}
}
func (uc *TorrentUseCase) storageFile(hash, path string, total *int64) {
	uc.storageLiveMu.Lock()
	defer uc.storageLiveMu.Unlock()
	if v := uc.storageLive[hash]; v != nil {
		v.progress.FilePath = path
		v.progress.FileBytesDone = 0
		v.progress.FileBytesTotal = total
	}
}
func (uc *TorrentUseCase) storageBytes(hash string, n int64) {
	if n <= 0 {
		return
	}
	if uc.storageByteHook != nil {
		uc.storageByteHook(hash, n)
	}
	uc.storageLiveMu.Lock()
	defer uc.storageLiveMu.Unlock()
	if v := uc.storageLive[hash]; v != nil {
		v.progress.BytesDone += n
		v.progress.FileBytesDone += n
		v.progress.LastProgressAt = time.Now().UnixMilli()
	}
}
func (uc *TorrentUseCase) storageFileDone(hash string) {
	uc.storageLiveMu.Lock()
	defer uc.storageLiveMu.Unlock()
	if v := uc.storageLive[hash]; v != nil {
		v.progress.FilesDone++
		v.progress.LastProgressAt = time.Now().UnixMilli()
	}
}
func (uc *TorrentUseCase) storageIORead(hash string) func(string, int64) {
	return func(path string, n int64) {
		rel := metadataRelative(uc.engine.DownloadDir(), path)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) > 2 && parts[0] == "payload" {
			rel = filepath.Join(parts[2:]...)
		}
		uc.storageLiveMu.Lock()
		if v := uc.storageLive[hash]; v != nil && v.progress.FilePath != rel {
			v.progress.FilePath = rel
			v.progress.FileBytesDone = 0
			v.progress.FileBytesTotal = nil
		}
		uc.storageLiveMu.Unlock()
		uc.storageBytes(hash, n)
	}
}
func (uc *TorrentUseCase) decorateStorage(t *models.Torrent) {
	if t == nil || t.Control == nil {
		return
	}
	uc.storageLiveMu.Lock()
	defer uc.storageLiveMu.Unlock()
	if live := uc.storageLive[t.Hash]; live != nil && live.recordID == t.Control.RecordID {
		if t.StorageRecovery == nil {
			t.StorageRecovery = &models.StorageRecovery{RecordID: live.recordID, Version: live.version, Reason: live.reason, Runtime: "absent", DataReadiness: "unknown", Actions: []models.StorageRecoveryAction{}}
		}
		if t.StorageRecovery.Version != live.version {
			return
		}
		p := live.progress
		// A snapshot sampler remains runnable even while the owning syscall blocks.
		// One timestamp source serves REST and latest-only SSE (sampled every 2s).
		p.HeartbeatAt = time.Now().UnixMilli()
		t.StorageRecovery.Progress = &p
	}
	if s := t.StorageRecovery; s != nil {
		if t.Control.RuntimeAttachment != "" {
			s.Runtime = "attached"
		}
		if t.ProgressSource == "verified" && t.Size > 0 && t.Downloaded >= t.Size {
			s.DataReadiness = "verified"
		} else if t.Activity.Verifying {
			s.DataReadiness = "checking"
		}
	}
}
func (uc *TorrentUseCase) decorateStorageList(all []*models.Torrent) {
	queued := int64(0)
	for _, t := range all {
		uc.decorateStorage(t)
		if t.Control == nil || t.DeletionMode != "" || t.StorageRecovery != nil {
			continue
		}
		if t.StorageState == "legacy" || t.StorageState == "blocked" || t.StorageState == "moving" {
			queued++
			reason := models.StorageMigrationQueued
			if t.StorageState == "blocked" {
				reason = models.StorageOwnershipBlocked
			}
			t.StorageRecovery = &models.StorageRecovery{RecordID: t.Control.RecordID, Version: 1, Reason: reason, Runtime: "absent", DataReadiness: "unknown", Actions: []models.StorageRecoveryAction{}, Progress: &models.StorageProgress{Stage: "queued", QueuePosition: int64ptr(queued), HeartbeatAt: time.Now().UnixMilli()}}
			if reason == models.StorageCatalogChanged || reason == models.StorageOwnershipBlocked {
				t.StorageRecovery.Actions = []models.StorageRecoveryAction{models.StorageRetryOwnership}
			}
		}
	}
	uc.storageLiveMu.Lock()
	defer uc.storageLiveMu.Unlock()
	for _, t := range all {
		if t.Control != nil && t.StorageRecovery != nil {
			if v := uc.migrationFailures[t.Hash]; v.recordID == t.Control.RecordID {
				t.StorageRecovery.RepeatCount = v.count
				if t.StorageRecovery.Reason == models.StorageMigrationQueued || t.StorageRecovery.Reason == models.StorageOwnershipBlocked {
					t.StorageRecovery.Reason = v.kind
					if v.kind == models.StorageCatalogChanged || v.kind == models.StorageOwnershipBlocked {
						t.StorageRecovery.Actions = []models.StorageRecoveryAction{models.StorageRetryOwnership}
					} else {
						t.StorageRecovery.Actions = []models.StorageRecoveryAction{}
					}
				}
			}
		}
	}
}

type observedReader struct {
	ctx     context.Context
	read    func([]byte) (int, error)
	observe func(int64)
}

func (r observedReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.read(p)
	if n > 0 && r.observe != nil {
		r.observe(int64(n))
	}
	return n, err
}

func metadataRelative(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return ""
	}
	return rel
}
