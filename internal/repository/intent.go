package repository

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"

	"torrent-stream-hub/internal/models"
)

func (r *TorrentRepo) GetIntent(hash string) (*models.TorrentIntent, error) {
	v := &models.TorrentIntent{}
	err := r.db.DB().QueryRow(`SELECT hash,record_id,revision,run_intent FROM torrent_intents WHERE hash=?`, hash).Scan(&v.Version.Hash, &v.Version.RecordID, &v.Version.Revision, &v.Run)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return v, err
}

// CommitRunIntent does not activate the engine, clear safety blocks or alter
// observations. The caller must coordinate commit/apply and surface apply errors.
func (r *TorrentRepo) CommitRunIntent(v models.IntentVersion, run models.RunIntent) (models.IntentVersion, error) {
	return r.commitRunIntent(v, run, false)
}

// CommitResumeRetry is used only by explicit Resume after application workflow
// checks and fail-closed staging. It atomically authorizes retry of legacy/missing
// payload failures, never verification or disk/workflow recovery.
func (r *TorrentRepo) CommitResumeRetry(v models.IntentVersion) (models.IntentVersion, error) {
	return r.commitRunIntent(v, models.RunEnabled, true)
}

func (r *TorrentRepo) commitRunIntent(v models.IntentVersion, run models.RunIntent, retry bool) (models.IntentVersion, error) {
	if !run.Valid() || v.Hash == "" || v.RecordID == "" || v.Revision < 1 || v.Revision == math.MaxInt64 {
		return v, fmt.Errorf("invalid intent command")
	}
	tx, err := r.db.BeginWrite()
	if err != nil {
		return v, err
	}
	defer tx.Rollback()
	if retry {
		if _, err := tx.Exec(`DELETE FROM torrent_activation_blocks WHERE record_id=? AND kind IN ('legacy_error','missing_files','metadata_persistence','invalid_selection')`, v.RecordID); err != nil {
			return v, err
		}
	}
	res, err := tx.Exec(`UPDATE torrent_intents SET run_intent=?,revision=revision+1
        WHERE hash=? AND record_id=? AND revision=?
        AND EXISTS(SELECT 1 FROM torrents t WHERE t.hash=torrent_intents.hash AND t.deletion_mode='')
        AND NOT EXISTS(SELECT 1 FROM torrent_storage s WHERE s.hash=torrent_intents.hash AND s.state='moving')
        AND (?='paused' OR NOT EXISTS(SELECT 1 FROM torrent_activation_blocks b WHERE b.record_id=torrent_intents.record_id))`, run, v.Hash, v.RecordID, v.Revision, run)
	if err != nil {
		return v, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return v, err
	}
	if n != 1 {
		return v, ErrStateConflict
	}
	// Compatibility projection for command paths still consuming torrents.state.
	// It is written atomically by the command, never from runtime observations.
	state := models.StateQueued
	if run == models.RunPaused {
		state = models.StatePaused
	}
	if _, err := tx.Exec(`UPDATE torrents SET state=?,error='' WHERE hash=?`, state, v.Hash); err != nil {
		return v, err
	}
	if err := tx.Commit(); err != nil {
		return v, err
	}
	v.Revision++
	return v, nil
}

// AttachRuntime invalidates every earlier attachment, even within the same
// process and intent revision. Tokens are issued only after the conditional write.
func (r *TorrentRepo) AttachRuntime(v models.IntentVersion) (models.RuntimeFence, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return models.RuntimeFence{}, err
	}
	f := models.RuntimeFence{IntentVersion: v, Attachment: hex.EncodeToString(token[:])}
	res, err := r.db.DB().Exec(`UPDATE torrent_intents SET runtime_attachment=?,observation_sequence=0,applied_revision=0
        WHERE hash=? AND record_id=? AND revision=? AND revision>0
        AND EXISTS(SELECT 1 FROM torrents t WHERE t.hash=torrent_intents.hash AND t.deletion_mode='')
        AND NOT EXISTS(SELECT 1 FROM torrent_storage s WHERE s.hash=torrent_intents.hash AND s.state='moving')`, f.Attachment, v.Hash, v.RecordID, v.Revision)
	if err != nil {
		return models.RuntimeFence{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return models.RuntimeFence{}, err
	}
	if n != 1 {
		return models.RuntimeFence{}, ErrStateConflict
	}
	return f, nil
}

func (r *TorrentRepo) SaveRuntimeObservation(o models.RuntimeObservation, contexts ...context.Context) error {
	if o.Fence.Hash == "" || o.Fence.RecordID == "" || o.Fence.Attachment == "" || o.Fence.Revision < 1 || o.Sequence < 1 || o.AppliedRevision < 0 || o.AppliedRevision > o.Fence.Revision || o.VerifiedBytes < 0 {
		return fmt.Errorf("invalid runtime observation")
	}
	verified := o.ProgressSource == "verified"
	if o.ProgressSource != "unknown" && o.ProgressSource != "checking" && !verified || verified && o.ObservedAt <= 0 {
		return fmt.Errorf("invalid observation provenance")
	}
	seen := make(map[int]bool, len(o.Files))
	var fileBytes int64
	for _, f := range o.Files {
		if f.Index < 0 || f.VerifiedBytes < 0 || seen[f.Index] || f.VerifiedBytes > math.MaxInt64-fileBytes {
			return fmt.Errorf("invalid file observation")
		}
		seen[f.Index] = true
		fileBytes += f.VerifiedBytes
	}
	tx, err := r.db.BeginWrite(contexts...)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE torrent_intents SET observation_sequence=?,applied_revision=?,observed_state=?,observed_error=?
        WHERE hash=? AND record_id=? AND revision=? AND runtime_attachment=? AND observation_sequence<?
        AND EXISTS(SELECT 1 FROM torrents t WHERE t.hash=torrent_intents.hash AND t.deletion_mode='')
        AND NOT EXISTS(SELECT 1 FROM torrent_storage s WHERE s.hash=torrent_intents.hash AND s.state='moving')`, o.Sequence, o.AppliedRevision, o.State, o.Error, o.Fence.Hash, o.Fence.RecordID, o.Fence.Revision, o.Fence.Attachment, o.Sequence)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrStaleObservation
	}
	// Sequence within the accepted attachment is authoritative even if the server
	// wall clock moves backwards. Timestamps remain diagnostic, not ordering tokens.
	if verified {
		var count int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM files WHERE hash=?`, o.Fence.Hash).Scan(&count); err != nil {
			return err
		}
		if count != len(o.Files) || fileBytes != o.VerifiedBytes {
			return fmt.Errorf("verified observation must cover the complete file catalog")
		}
		res, err = tx.Exec(`UPDATE torrents SET downloaded=?,progress_source='verified',progress_observed_at=? WHERE hash=? AND ?>=0 AND ?<=size`, o.VerifiedBytes, o.ObservedAt, o.Fence.Hash, o.VerifiedBytes, o.VerifiedBytes)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("verified bytes exceed catalog size")
		}
	}
	// Validate the catalog once; unchanged observations do not update file rows.
	rows, err := tx.Query(`SELECT "index",size,downloaded FROM files WHERE hash=?`, o.Fence.Hash)
	if err != nil {
		return err
	}
	type fileValue struct{ size, bytes int64 }
	values := make(map[int]fileValue, len(o.Files))
	for rows.Next() {
		var index int
		var v fileValue
		if err := rows.Scan(&index, &v.size, &v.bytes); err != nil {
			rows.Close()
			return err
		}
		values[index] = v
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, f := range o.Files {
		current, ok := values[f.Index]
		if !ok || f.VerifiedBytes > current.size {
			return ErrFileNotFound
		}
		if verified && f.VerifiedBytes != current.bytes {
			if _, err := tx.Exec(`UPDATE files SET downloaded=? WHERE hash=? AND "index"=?`, f.VerifiedBytes, o.Fence.Hash, f.Index); err != nil {
				return err
			}
		}
		if verified && f.VerifiedBytes > 0 {
			// Current attachment proof creates an expectation, never a resume
			// capability. Unchanged expectations do not generate telemetry writes.
			_, err := tx.Exec(`INSERT INTO file_inventory(hash,record_id,file_index,mapping_revision,metadata_path,size,expected,presence,reason,observed_at_ms)
 SELECT f.hash,?,f."index",s.revision,f.path,f.size,1,'present','',? FROM files f JOIN torrent_storage s ON s.hash=f.hash WHERE f.hash=? AND f."index"=? AND s.state='ready'
 ON CONFLICT(hash,file_index) DO UPDATE SET record_id=excluded.record_id,mapping_revision=excluded.mapping_revision,metadata_path=excluded.metadata_path,size=excluded.size,expected=1,presence='present',reason='',observed_at_ms=excluded.observed_at_ms
 WHERE file_inventory.expected=0 OR file_inventory.record_id!=excluded.record_id OR file_inventory.mapping_revision!=excluded.mapping_revision OR file_inventory.metadata_path!=excluded.metadata_path OR file_inventory.size!=excluded.size`, o.Fence.RecordID, o.ObservedAt/1000000, o.Fence.Hash, f.Index)
			if err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}

// Blocks have independent keys: successful verification cannot clear DiskFull,
// MissingFiles, deletion or migration. These methods never mutate run intent.
func (r *TorrentRepo) PutActivationBlock(v models.RuntimeFence, kind models.ActivationBlockKind, reason string) error {
	if !kind.Valid() || reason == "" || v.Attachment == "" {
		return fmt.Errorf("invalid activation block")
	}
	res, err := r.db.DB().Exec(`INSERT INTO torrent_activation_blocks(record_id,kind,reason)
        SELECT record_id,?,? FROM torrent_intents WHERE hash=? AND record_id=? AND revision=? AND runtime_attachment=?
		ON CONFLICT(record_id,kind) DO UPDATE SET reason=excluded.reason`, kind, reason, v.Hash, v.RecordID, v.Revision, v.Attachment)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrStateConflict
	}
	return nil
}

// PutRuntimeSafetyBlock is for current-attachment physical/application faults,
// independent of a command revision. A command can commit while its runtime is
// still attached to the preceding revision; that must not erase safety evidence.
// Unlike verifier completion, these producers only ADD a key, never clear one.
// A replaced attachment/record cannot write even if the hash is unchanged.
func (r *TorrentRepo) PutRuntimeSafetyBlock(v models.RuntimeFence, kind models.ActivationBlockKind, reason string) error {
	if (kind != models.BlockMetadataPersistence && kind != models.BlockInvalidSelection && kind != models.BlockMissingFiles) || reason == "" || v.Attachment == "" || v.Revision < 1 {
		return fmt.Errorf("invalid runtime safety block")
	}
	res, err := r.db.DB().Exec(`INSERT INTO torrent_activation_blocks(record_id,kind,reason)
		SELECT record_id,?,? FROM torrent_intents WHERE hash=? AND record_id=? AND revision>=? AND runtime_attachment=?
		ON CONFLICT(record_id,kind) DO UPDATE SET reason=excluded.reason`, kind, reason, v.Hash, v.RecordID, v.Revision, v.Attachment)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrStateConflict
	}
	return nil
}

func (r *TorrentRepo) ClearActivationBlock(v models.RuntimeFence, kind models.ActivationBlockKind) error {
	if !kind.Valid() || v.Attachment == "" {
		return fmt.Errorf("invalid activation block kind")
	}
	tx, err := r.db.BeginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRow(`SELECT 1 FROM torrent_intents WHERE hash=? AND record_id=? AND revision=? AND runtime_attachment=?`, v.Hash, v.RecordID, v.Revision, v.Attachment).Scan(&exists); err == sql.ErrNoRows {
		return ErrStateConflict
	} else if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM torrent_activation_blocks WHERE record_id=? AND kind=?`, v.RecordID, kind); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *TorrentRepo) ActivationBlocks(v models.IntentVersion) ([]models.ActivationBlock, error) {
	rows, err := r.db.DB().Query(`SELECT b.kind,b.reason FROM torrent_activation_blocks b JOIN torrent_intents i ON i.record_id=b.record_id WHERE i.hash=? AND i.record_id=? AND i.revision=? ORDER BY b.kind`, v.Hash, v.RecordID, v.Revision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.ActivationBlock
	for rows.Next() {
		var b models.ActivationBlock
		if err := rows.Scan(&b.Kind, &b.Reason); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
