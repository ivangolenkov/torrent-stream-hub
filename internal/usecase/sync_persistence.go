package usecase

import (
	"context"
	"errors"
	"time"

	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"
)

type persistenceJob struct {
	fence  models.RuntimeFence
	stamp  engine.RuntimeReadStamp
	record string
}

func (sw *SyncWorker) enqueuePersistence(items []*models.Torrent) {
	sw.persistMu.Lock()
	defer sw.persistMu.Unlock()
	if sw.persistCtx.Err() != nil {
		return
	}
	for sw.persistWorkers < 2 {
		sw.persistWG.Add(1)
		if !sw.engine.StartTask(func(context.Context) { defer sw.persistWG.Done(); sw.persistLoop() }) {
			sw.persistWG.Done()
			logging.Warnf("observation worker admission rejected")
			break
		}
		sw.persistWorkers++
	}

	for _, t := range items {
		if t.Control == nil {
			continue
		}
		raw, f := sw.engine.RuntimeBinding(t.Hash)
		if raw == nil {
			continue
		}
		if f.RecordID != "" && (f.RecordID != t.Control.RecordID || f.Attachment != t.Control.RuntimeAttachment) {
			continue
		}
		if _, exists := sw.persistPending[t.Hash]; !exists && len(sw.persistPending) >= 128 {
			continue
		}
		f.Hash = t.Hash
		sw.persistPending[t.Hash] = persistenceJob{fence: f, stamp: sw.engine.CaptureReadStamp(t.Hash), record: t.Control.RecordID}
	}
	for range 2 {
		select {
		case sw.persistWake <- struct{}{}:
		default:
		}
	}
}

func (sw *SyncWorker) persistLoop() {
	for {
		select {
		case <-sw.persistCtx.Done():
			return
		case <-sw.persistWake:
		}
		for sw.persistCtx.Err() == nil {
			sw.persistMu.Lock()
			var job persistenceJob
			for hash, pending := range sw.persistPending {
				job = pending
				delete(sw.persistPending, hash)
				break
			}
			sw.persistMu.Unlock()
			if job.fence.Hash == "" {
				break
			}
			ctx, cancel := context.WithTimeout(sw.persistCtx, 2*time.Second)
			err := sw.persistRecord(ctx, job)
			cancel()
			if err != nil && !errors.Is(err, errCoordinatorBusy) && !errors.Is(err, context.Canceled) {
				logging.Warnf("observation persistence postponed hash=%s: %v", job.fence.Hash, err)
			}
			// The next periodic sample retries current state. No retained DTO or
			// recursive retry can grow the queue or supersede a command fence.
		}
	}
}

func (sw *SyncWorker) persistRecord(ctx context.Context, job persistenceJob) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	release, err := sw.uc.tryLockRecord(job.fence.Hash)
	if err != nil {
		return err
	}
	defer release()
	if !sw.engine.ValidateReadStamp(job.stamp) {
		return nil
	}
	t, err := sw.repo.GetTorrentContext(ctx, job.fence.Hash)
	if err != nil || t == nil {
		return err
	}
	if t.Control == nil || t.Control.RecordID != job.record {
		return nil
	}
	if _, pending, err := sw.repo.CleanupPaths(t.Hash); err != nil || pending {
		return err
	}
	_, err = capturePersistableRuntime(sw.engine, sw.repo, t.Hash, ctx)
	return err
}

// CompactTorrents is an explicit wire projection. Catalog/details are loaded
// separately by catalog version and record/revision, never used as readiness.
func CompactTorrents(items []*models.Torrent) []*models.Torrent {
	result := make([]*models.Torrent, 0, len(items))
	for _, item := range items {
		copy := *item
		copy.Files = nil
		copy.Data = ""
		result = append(result, &copy)
	}
	return result
}
