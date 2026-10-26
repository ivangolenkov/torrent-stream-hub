package repository

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strconv"

	"github.com/anacrolix/torrent/storage"
	"torrent-stream-hub/internal/fastresume"
)

func (r *TorrentRepo) ResumeOwnerMapping(ctx context.Context, storageID string) (*StorageMapping, string, error) {
	// One coherent, cancellable read: a final dirty scan must not drop its
	// budget while waiting for another non-context-aware pool acquisition.
	m := new(StorageMapping)
	var record string
	err := r.db.DB().QueryRowContext(ctx, `SELECT s.hash,s.version,s.root,s.state,s.manifest,s.error,s.storage_id,s.revision,s.prefix,s.name_hint,i.record_id
		FROM torrent_storage s JOIN torrent_intents i ON i.hash=s.hash
		WHERE s.storage_id=? AND (SELECT count(*) FROM torrent_storage a JOIN torrent_intents b ON b.hash=a.hash WHERE a.storage_id=s.storage_id)=1`, storageID).
		Scan(&m.Hash, &m.Version, &m.Root, &m.State, &m.Manifest, &m.Error, &m.StorageID, &m.Revision, &m.Prefix, &m.NameHint, &record)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrStateConflict
	}
	if err != nil {
		return nil, "", err
	}
	return m, record, nil
}

// New torrent registration may open its prepared mapping before its first
// intent is committed. Only an exact mapping with NO record owner and NO live
// checkpoint authority can cross that initial untrusted namespace boundary.
func (r *TorrentRepo) ResumeUnboundMutationSafe(ctx context.Context, m *StorageMapping) (bool, error) {
	if m == nil {
		return false, ErrStateConflict
	}
	tx, err := r.db.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var exact, owners int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM torrent_storage WHERE hash=? AND storage_id=? AND revision=? AND root=? AND prefix=? AND version=?`, m.Hash, m.StorageID, m.Revision, m.Root, m.Prefix, m.Version).Scan(&exact); err != nil {
		return false, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM torrent_intents i JOIN torrent_storage s ON s.hash=i.hash WHERE s.storage_id=?`, m.StorageID).Scan(&owners); err != nil {
		return false, err
	}
	head, err := readResumeHead(contextReadTx{Tx: tx, ctx: ctx}, m.StorageID)
	if err != nil {
		return false, err
	}
	return exact == 1 && owners == 0 && (head == nil || head.Invalidated), tx.Commit()
}

// InvalidateResumeMutation is the durable namespace/recheck barrier. Unlike
// checkpoint admission it intentionally accepts an active recovery journal and
// deletion intent: those workflows must revoke BEFORE their filesystem effects.
// Exact current record/mapping fencing still applies. The caller owns existing
// application mutation reservations and retains them through the effect/join.
func (r *TorrentRepo) InvalidateResumeMutation(ctx context.Context, m *StorageMapping, record string) error {
	if m == nil || record == "" {
		return ErrStateConflict
	}
	tx, err := r.db.BeginWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkResumeDurability(tx); err != nil {
		return err
	}
	var owners int
	if err := tx.QueryRow(`SELECT count(*) FROM torrent_storage s JOIN torrent_intents i ON i.hash=s.hash WHERE s.hash=? AND i.record_id=? AND s.storage_id=? AND s.revision=? AND s.root=? AND s.prefix=? AND s.version=?`, m.Hash, record, m.StorageID, m.Revision, m.Root, m.Prefix, m.Version).Scan(&owners); err != nil {
		return err
	}
	if owners != 1 {
		return ErrStateConflict
	}
	head, err := readResumeHead(tx, m.StorageID)
	if err != nil {
		return err
	}
	if head != nil {
		if head.Token.Epoch == math.MaxInt64 {
			return ErrStateConflict
		}
		// Even an invalidated head may have a captured but unpublished seal.
		// Advance its epoch rather than letting that callback clear this barrier.
		if _, err := tx.Exec(`UPDATE resume_owners SET invalidated=1,mutation_epoch=mutation_epoch+1 WHERE storage_id=?`, m.StorageID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// NewResumeMutationHooks binds one backend open to exact durable ownership. The
// backend coalesces its persisted bitmap, so ordinary untrusted chunks perform
// no SQL. strict revokes the previous checkpoint before a verify-mode open.
func (r *TorrentRepo) NewResumeMutationHooks(ctx context.Context, mapping *StorageMapping, record string, pieces int, pieceLength int64, strict bool) (*storage.FileResumeHooks, error) {
	if mapping == nil {
		return nil, ErrStateConflict
	}
	m := *mapping
	if pieces < 0 || pieces > fastresume.MaxPieces {
		if err := r.InvalidateResumeMutation(ctx, &m, record); err != nil {
			return nil, err
		}
		return nil, nil // oversized ordinary verification, never writable import
	}
	hooks := &storage.FileResumeHooks{Pending: make([]byte, (pieces+7)/8)}
	hooks.Observation.Status, hooks.Observation.Reason = "fallback", "no_checkpoint"
	hooks.Invalidate = func() error { return r.InvalidateResumeMutation(ctx, &m, record) }
	hooks.Revoke = func(mask []byte) (bool, error) { return r.RevokeResumeMutation(ctx, &m, record, mask) }
	if strict {
		hooks.Observation.Status, hooks.Observation.Reason = "disabled", "strict_mode"
		// Recovery's explicit verified-resume may open storage while its journal
		// is still active. It must revoke, but must NOT request checkpoint
		// admission (which correctly rejects that journal).
		if err := hooks.Invalidate(); err != nil {
			return nil, err
		}
		return hooks, nil
	}
	// Rebind only through the repository's exact CURRENT record/mapping fence;
	// an old retained tombstone must not block ordinary verification after move
	// or re-add, and must never become authority for the new attachment.
	if _, err := r.CaptureResume(ctx, &m, record); err != nil {
		return nil, err
	}
	saved, err := r.LoadResume(ctx, &m, record)
	if err != nil && !errors.Is(err, ErrResumeIntegrity) && !errors.Is(err, fastresume.ErrInvalid) && !errors.Is(err, fastresume.ErrBudget) {
		return nil, err
	}
	if err != nil || saved != nil && !saved.Invalidated && (saved.Checkpoint.Identity.PieceCount != pieces || saved.Checkpoint.Identity.PieceLength != pieceLength) {
		hooks.Observation.Reason = "invalid_checkpoint"
		if errors.Is(err, fastresume.ErrBudget) {
			hooks.Observation.Reason = "budget_exceeded"
		}
		if errors.Is(err, ErrResumeIntegrity) {
			hooks.Observation.Reason = "checkpoint_integrity_error"
		}
		if err := hooks.Invalidate(); err != nil {
			return nil, err
		}
		return hooks, nil
	}
	if saved != nil && !saved.Invalidated {
		hooks.Observation.Generation = strconv.FormatInt(saved.Token.Generation, 10)
		hooks.Observation.CheckpointAtMS = saved.Checkpoint.CreatedAtMS
		hooks.Observation.Status, hooks.Observation.Reason = "pending", "restored"
		hooks.RestoreCandidate = true
		for i := range hooks.Pending {
			hooks.Pending[i] = saved.Checkpoint.Pieces[i] &^ saved.Revoked[i]
			if saved.Revoked[i] != 0 {
				hooks.Observation.Reason = "pieces_revoked"
			}
		}
	}
	return hooks, nil
}

// RevokeResumeMutation is used by the backend only for its still-persisted mask,
// under the backend lifecycle fence. New/untrusted chunks never call it. A
// corrupt checkpoint can be safely discarded, but no write is permitted on an
// ambiguous/stale durability fence.
func (r *TorrentRepo) RevokeResumeMutation(ctx context.Context, m *StorageMapping, record string, pieces []byte) (bool, error) {
	saved, err := r.LoadResume(ctx, m, record)
	if err != nil {
		return false, err
	}
	if saved == nil {
		return true, nil
	}
	if saved.Invalidated {
		return true, nil
	}
	if err := fastresume.ValidateBitmap(pieces, saved.Checkpoint.Identity.PieceCount); err != nil {
		return false, err
	}
	_, err = r.RevokeResume(ctx, m, saved.Token, pieces)
	return false, err
}
