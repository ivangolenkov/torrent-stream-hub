package repository

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"torrent-stream-hub/internal/fastresume"
)

// ResumeSource is an attachment-scoped capability supplied while the application
// owns the existing record/storage/root reservations. Authorize must revalidate
// that attachment (including volume/inventory/safety gates), not acquire another
// command reservation. Capture is normally Torrent.WithFileCheckpoint. Metadata
// and Identity belong to the immutable canonical owner, never a hash alias.
// A source is not safe to retain past release of the application's reservations.
type ResumeSource struct {
	Identity  fastresume.Identity
	Metadata  *metainfo.MetaInfo
	Info      *metainfo.Info // immutable validated Info of this same attachment
	Authorize func(context.Context) error
	Capture   func(context.Context, func(storage.FileCheckpoint) error) error
}

func (s ResumeSource) validate(m *StorageMapping) (*metainfo.Info, error) {
	if m == nil || s.Metadata == nil || s.Info == nil || s.Authorize == nil || s.Identity.StorageID != m.StorageID || s.Identity.MappingRevision != m.Revision || s.Identity.Root != m.Root || s.Identity.Prefix != m.Prefix || s.Identity.LayoutVersion != m.Version {
		return nil, fastresume.ErrInvalid
	}
	return s.Info, nil
}

// SealResume implements capture -> rooted flush -> durable atomic publication.
// The source's application reservations remain owned for this entire call; no
// SQL connection/transaction is held while Capture quiesces/flushes payload.
// Production use additionally requires mutation hooks before reopening writers.
func (r *TorrentRepo) SealResume(ctx context.Context, m *StorageMapping, source ResumeSource) error {
	info, err := source.validate(m)
	if err != nil {
		return err
	}
	if source.Capture == nil {
		return fastresume.ErrInvalid
	}
	if err := source.Authorize(ctx); err != nil {
		return err
	}
	token, err := r.CaptureResume(ctx, m, source.Identity.RecordID)
	if err != nil {
		return err
	}
	if token.Generation == math.MaxInt64 {
		return fastresume.ErrInvalid
	}
	return source.Capture(ctx, func(native storage.FileCheckpoint) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := source.Authorize(ctx); err != nil {
			return err
		}
		checkpoint, err := fastresume.FromStorage(source.Identity, source.Metadata, info, token.Generation+1, time.Now().UnixMilli(), native)
		if err != nil {
			return err
		}
		_, err = r.CommitResume(ctx, m, token, checkpoint)
		return err
	})
}

// RestoreResume never consumes a persisted checkpoint. It loads a coherent,
// integrity-checked snapshot, subtracts durable revocations, and asks the backend
// for an attachment/epoch-scoped one-use token after rooted evidence validation.
// Failure grants no readiness; callers use ordinary verification as fallback.
// Writable import is still rejected by the backend until mutation hooks exist.
func (r *TorrentRepo) RestoreResume(ctx context.Context, m *StorageMapping, source ResumeSource, backend storage.TorrentImpl) error {
	// Rooted evidence validation can durably revoke a changed file via the
	// writable backend hooks. Reload its new epoch once instead of treating our
	// own safety barrier as loss of every independent file's checkpoint.
	for attempt := 0; attempt < 2; attempt++ {
		err := r.restoreResumeOnce(ctx, m, source, backend)
		if attempt == 0 && (errors.Is(err, ErrStateConflict) || errors.Is(err, storage.ErrCheckpointChanged)) {
			continue
		}
		return err
	}
	return ErrStateConflict
}

func (r *TorrentRepo) restoreResumeOnce(ctx context.Context, m *StorageMapping, source ResumeSource, backend storage.TorrentImpl) error {
	info, err := source.validate(m)
	if err != nil {
		return err
	}
	if backend.PrepareRestore == nil || backend.RestoreCheckpoint == nil {
		return storage.ErrCheckpointUnsupported
	}
	if err := source.Authorize(ctx); err != nil {
		return err
	}
	saved, err := r.LoadResume(ctx, m, source.Identity.RecordID)
	if err != nil {
		return err
	}
	if saved == nil || saved.Invalidated || saved.Checkpoint == nil {
		return ErrStateConflict
	}
	claims, err := fastresume.StorageClaims(source.Identity, source.Metadata, info, saved.Checkpoint, saved.Revoked)
	if err != nil {
		return err
	}
	authorize := func(ctx context.Context) error {
		if err := source.Authorize(ctx); err != nil {
			return err
		}
		latest, err := r.LoadResume(ctx, m, source.Identity.RecordID)
		if err != nil {
			return err
		}
		if latest == nil || latest.Invalidated || latest.Token != saved.Token {
			return ErrStateConflict
		}
		return nil
	}
	token, err := backend.PrepareRestore(ctx, claims, authorize)
	if err != nil {
		return err
	}
	return backend.RestoreCheckpoint(ctx, token)
}

// ResumeLookup supplies and releases existing application ownership, not a second
// lock registry. The worker is only scheduling: lookup must validate current
// identity and keep all reservations through SealResume, including failed I/O.
type ResumeLookup func(context.Context, string) (*StorageMapping, ResumeSource, func(), error)

func (r *TorrentRepo) NewResumeProducer(lookup ResumeLookup) (*fastresume.SealWorker, error) {
	if lookup == nil {
		return nil, errors.New("nil resume owner lookup")
	}
	return fastresume.NewSealWorker(func(ctx context.Context, owner string) error {
		mapping, source, release, err := lookup(ctx, owner)
		if release != nil {
			defer release()
		}
		if err != nil {
			return err
		}
		if release == nil || mapping == nil || mapping.StorageID != owner {
			return ErrStateConflict
		}
		return r.SealResume(ctx, mapping, source)
	})
}
