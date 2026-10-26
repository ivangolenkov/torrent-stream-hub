package usecase

import (
	"context"
	"errors"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"torrent-stream-hub/internal/fastresume"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"
)

func resumeFileCount(info *metainfo.Info) (int, error) {
	if info == nil {
		return 0, fastresume.ErrInvalid
	}
	count := 0
	for range info.UpvertedFilesIter() {
		count++
		if count > fastresume.MaxFiles {
			return 0, fastresume.ErrBudget
		}
	}
	return count, nil
}

func (uc *TorrentUseCase) installResumeProducer() error {
	p, err := newApplicationResumeProducer(uc, uc.productionResumeSource)
	if err != nil {
		return err
	}
	p.finalScan = uc.finalResumeDirtyScan
	if !uc.engine.RegisterStorageFinalizer(p) {
		// Ownership did not transfer. Join only the new worker, without
		// closing the application's coordinator or touching the existing slot.
		err := p.worker.Shutdown(context.Background())
		return errors.Join(errors.New("resume finalizer registration rejected"), err, p.worker.Wait())
	}
	uc.resumeProducer = p
	if !uc.engine.StartTask(func(ctx context.Context) { uc.runResumeDirtyScanner(ctx, p) }) {
		// The finalizer owns p now; Engine.Close is its only shutdown owner.
		return context.Canceled
	}
	return nil
}

func (uc *TorrentUseCase) finalResumeDirtyScan(ctx context.Context, mark func(string) bool) error {
	// Catch changes since the last periodic scan without an unbounded shutdown
	// library walk. Leave most of the shared ten-second budget for flush/seal.
	scanCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	cursor, full := "", false
	for {
		next, err := uc.scanResumeDirty(scanCtx, cursor, func(owner string) bool {
			accepted := mark(owner)
			full = full || !accepted
			return accepted
		})
		if err != nil {
			// This scan is lossy scheduling, not a safety barrier. Preserve the
			// queued work and its independent drain budget on scan timeout.
			return nil
		}
		if next == "" || full {
			return nil
		}
		cursor = next
	}
}

// resumeVolumeToken only selects cached registration identity. Authorize must
// still check the rooted marker and device boundary before every capture/commit.
func (uc *TorrentUseCase) resumeVolumeToken(path string) string {
	uc.volumeMu.RLock()
	defer uc.volumeMu.RUnlock()
	best, token := -1, ""
	for _, v := range uc.volumes {
		if v.State == "ready" && (v.Root == "." || path == v.Root || payload.Overlap(path, v.Root) && len(path) > len(v.Root)) && len(v.Root) > best {
			best, token = len(v.Root), v.Token
		}
	}
	return token
}

// productionResumeSource is resolved again after command reservations are held.
// Its closure captures exact attachment identity, not just a hash. No callback
// reenters the library or acquires command keys while storage is quiesced.
func (uc *TorrentUseCase) productionResumeSource(ctx context.Context, owner string) (string, *repository.StorageMapping, repository.ResumeSource, error) {
	var source repository.ResumeSource
	m, record, err := uc.repo.ResumeOwnerMapping(ctx, owner)
	if err != nil {
		return "", nil, source, err
	}
	raw, fence, applied := uc.engine.RuntimeAppliedBinding(m.Hash)
	if raw == nil || fence.RecordID != record || fence.Attachment == "" {
		return "", nil, source, ErrStorageConflict
	}
	info := raw.Info()
	if _, err := resumeFileCount(info); err != nil {
		return "", nil, source, err
	}
	if info.NumPieces() > fastresume.MaxPieces {
		return "", nil, source, fastresume.ErrBudget
	}
	metadata, err := raw.FileCheckpointMetainfo()
	if err != nil {
		return "", nil, source, err
	}
	volume := uc.resumeVolumeToken(m.Root)
	if volume == "" {
		return "", nil, source, payload.ErrVolumeUnregistered
	}
	source = repository.ResumeSource{
		Identity: fastresume.Identity{RecordID: record, StorageID: owner, MappingRevision: m.Revision, LayoutVersion: m.Version, Root: m.Root, Prefix: m.Prefix, VolumeID: volume},
		Metadata: &metadata, Info: info, Capture: raw.WithFileCheckpoint,
	}
	expected := *m
	source.Authorize = func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, bound, revision := uc.engine.RuntimeAppliedBinding(expected.Hash)
		if current != raw || bound != fence || revision != applied {
			return ErrStorageConflict
		}
		intent, err := uc.repo.GetIntent(expected.Hash)
		if err != nil {
			return err
		}
		if intent == nil || intent.Version != fence.IntentVersion {
			return ErrStorageConflict
		}
		if err := uc.repo.CheckResumeAdmission(ctx, &expected, record); err != nil {
			return err
		}
		blocks, err := uc.repo.ActivationBlocks(intent.Version)
		if err != nil {
			return err
		}
		live, err := uc.engine.RuntimeSafetyBlocks(raw, expected.Hash)
		if err != nil {
			return err
		}
		disk, err := uc.repo.DownloadBlock(expected.Hash)
		if err != nil {
			return err
		}
		if len(blocks) != 0 || len(live) != 0 || disk != "" {
			return ErrStorageConflict
		}
		if err := uc.inventoryAdmitted(&expected); err != nil {
			return err
		}
		if uc.resumeVolumeToken(expected.Root) != volume {
			return ErrStorageConflict
		}
		// Paused intent and orderly engine cancellation are not corruption.
		// The independent job ctx and drain reservations govern shutdown seals.
		return uc.checkVolume(expected.Root)
	}
	return m.Hash, m, source, nil
}

// scanResumeDirty samples only cached runtime revisions for a bounded owner page.
// Rejected queue entries are revisited on the next round; durable revocations do
// not depend on this scheduling hint. No stat/hash/flush or checkpoint SQL here.
func (uc *TorrentUseCase) scanResumeDirty(ctx context.Context, cursor string, mark func(string) bool) (string, error) {
	owners, err := uc.repo.ResumeOwnersAfter(ctx, cursor)
	if err != nil {
		return cursor, err
	}
	for _, owner := range owners {
		m, _, err := uc.repo.ResumeOwnerMapping(ctx, owner)
		if err != nil {
			if ctx.Err() != nil {
				return cursor, ctx.Err()
			}
			continue
		}
		if raw := uc.engine.GetRawTorrent(m.Hash); raw != nil {
			if _, dirty := raw.FileCheckpointState(); dirty {
				mark(owner)
			}
		}
	}
	if len(owners) < fastresume.MaxPendingOwners {
		return "", nil
	}
	return owners[len(owners)-1], nil
}

func (uc *TorrentUseCase) runResumeDirtyScanner(ctx context.Context, producer *applicationResumeProducer) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	cursor := ""
	for {
		var err error
		cursor, err = uc.scanResumeDirty(ctx, cursor, producer.MarkDirty)
		if err != nil && ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
