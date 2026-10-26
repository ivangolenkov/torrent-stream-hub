package repository

import (
	"context"
	"database/sql"
	"errors"
	"torrent-stream-hub/internal/payload"
)

var ErrStorageQueueFull = errors.New("storage queue full")

type StorageClaim struct {
	StorageID, RecordID string
	Version             int64
	Path, Kind          string
}

// CreateStorageOperation publishes a journal and all its namespace claims in a
// single short transaction. The caller has completed filesystem preflight.
func (r *TorrentRepo) CreateStorageOperation(ctx context.Context, m *StorageMapping, v *RecoveryRecord, previousVersion int64, claims []StorageClaim) error {
	tx, err := r.db.BeginWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkStorageFence(tx, m, v.RecordID); err != nil {
		return err
	}
	var current int64
	err = tx.QueryRow(`SELECT version FROM storage_recovery WHERE hash=? AND record_id=?`, v.Hash, v.RecordID).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if current != previousVersion || v.Version != current+1 || v.Version < 1 {
		return ErrStateConflict
	}
	var pending int
	if err := tx.QueryRow(`SELECT count(*) FROM storage_recovery WHERE reason IN ('storage_move','storage_locate') AND stage NOT IN ('planned','analyzed','completed','cancelled')`).Scan(&pending); err != nil {
		return err
	}
	if pending >= 64 {
		return ErrStorageQueueFull
	}
	for _, c := range claims {
		if c.StorageID != m.StorageID || c.RecordID != v.RecordID || c.Version != v.Version || payload.ValidateRelative(c.Path) != nil {
			return ErrStateConflict
		}
		if err := checkStorageClaims(tx, c.StorageID, c.Path); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`INSERT INTO storage_recovery(hash,record_id,version,reason,stage,journal,location_source,location_target,location_method,location_extra_bytes,location_alternatives,location_alternative_bytes,location_analysis,location_free_bytes) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(hash) DO UPDATE SET version=excluded.version,reason=excluded.reason,stage=excluded.stage,journal=excluded.journal,error='',failure_code='',location_source=excluded.location_source,location_target=excluded.location_target,location_method=excluded.location_method,location_extra_bytes=excluded.location_extra_bytes,location_alternatives=excluded.location_alternatives,location_alternative_bytes=excluded.location_alternative_bytes,location_analysis=excluded.location_analysis,location_free_bytes=excluded.location_free_bytes`, v.Hash, v.RecordID, v.Version, v.Reason, v.Stage, v.Journal, v.LocationSource, v.LocationTarget, v.LocationMethod, v.LocationExtraBytes, v.LocationAlternatives, v.LocationAlternativeBytes, v.LocationAnalysis, v.LocationFreeBytes)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM storage_claims WHERE record_id=? AND kind!='retained'`, v.RecordID); err != nil {
		return err
	}
	for _, c := range claims {
		if _, err := tx.Exec(`INSERT INTO storage_claims(storage_id,record_id,operation_version,path,kind) VALUES(?,?,?,?,?)`, c.StorageID, c.RecordID, c.Version, c.Path, c.Kind); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func checkStorageFence(tx sqlReader, m *StorageMapping, id string) error {
	var n int
	err := tx.QueryRow(`SELECT count(*) FROM torrent_storage s JOIN torrent_intents i ON i.hash=s.hash JOIN torrents t ON t.hash=s.hash WHERE s.hash=? AND i.record_id=? AND s.storage_id=? AND s.revision=? AND s.root=? AND s.prefix=? AND s.version=? AND t.deletion_mode=''`, m.Hash, id, m.StorageID, m.Revision, m.Root, m.Prefix, m.Version).Scan(&n)
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrStateConflict
	}
	return nil
}

func checkStorageClaims(tx *writeTx, owner, path string) error {
	rows, err := tx.Query(`SELECT storage_id,path FROM storage_claims UNION ALL SELECT storage_id,root FROM torrent_storage WHERE root!=''`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, other string
		if err := rows.Scan(&id, &other); err != nil {
			return err
		}
		if id != owner && payload.Overlap(payload.CollisionKey(path), payload.CollisionKey(other)) {
			return ErrStateConflict
		}
	}
	return rows.Err()
}

// TransitionStorageOperation is also the cancellation linearization point.
// Publication and cancellation race on an exact stage, never on a stale copy.
func (r *TorrentRepo) TransitionStorageOperation(m *StorageMapping, v *RecoveryRecord, from string) error {
	tx, err := r.db.BeginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkStorageFence(tx, m, v.RecordID); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE storage_recovery SET stage=?,journal=?,error=?,failure_code=?,location_source=?,location_target=?,location_method=?,location_extra_bytes=?,location_alternatives=?,location_alternative_bytes=?,location_analysis=?,location_free_bytes=? WHERE hash=? AND record_id=? AND version=? AND reason=? AND stage=?`, v.Stage, v.Journal, v.Error, v.FailureCode, v.LocationSource, v.LocationTarget, v.LocationMethod, v.LocationExtraBytes, v.LocationAlternatives, v.LocationAlternativeBytes, v.LocationAnalysis, v.LocationFreeBytes, v.Hash, v.RecordID, v.Version, v.Reason, from)
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
	return tx.Commit()
}

func (r *TorrentRepo) PendingStorageOperations(ctx context.Context) ([]string, error) {
	rows, err := r.db.DB().QueryContext(ctx, `SELECT hash FROM storage_recovery WHERE reason IN ('storage_move','storage_locate') AND stage NOT IN ('planned','analyzed','completed','cancelled') AND error='' ORDER BY rowid LIMIT 64`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		hashes = append(hashes, hash)
	}
	return hashes, rows.Err()
}

// CommitStorageOperation is the FS/SQL hand-off: aliases, retained source and
// runtime fencing change in the same transaction as the publication checkpoint.
func (r *TorrentRepo) CommitStorageOperation(ctx context.Context, before *StorageMapping, v *RecoveryRecord, next payload.Layout, paused bool) error {
	tx, err := r.db.BeginWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkStorageFence(tx, before, v.RecordID); err != nil {
		return err
	}
	var stage string
	if err := tx.QueryRow(`SELECT stage FROM storage_recovery WHERE hash=? AND record_id=? AND version=? AND reason=?`, v.Hash, v.RecordID, v.Version, v.Reason).Scan(&stage); err != nil {
		return err
	}
	if stage != "published" {
		return ErrStateConflict
	}
	if err := setStorageLayoutTx(tx, before, v.RecordID, next); err != nil {
		return err
	}
	if paused { // Locate starts a new presence baseline, without adopting completion.
		if _, err := tx.Exec(`DELETE FROM file_inventory WHERE record_id=?`, v.RecordID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM torrent_activation_blocks WHERE record_id=? AND kind='missing_files' AND reason='Expected file absent during inventory'`, v.RecordID); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`UPDATE storage_recovery SET stage='committed',error='',failure_code='',journal=? WHERE hash=? AND record_id=? AND version=?`, v.Journal, v.Hash, v.RecordID, v.Version)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE storage_claims SET kind='retained' WHERE record_id=? AND operation_version=? AND kind='source'`, v.RecordID, v.Version)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE torrent_intents SET runtime_attachment='',observation_sequence=0,applied_revision=0,run_intent=CASE WHEN ? THEN 'paused' ELSE run_intent END,revision=revision+CASE WHEN ? AND run_intent!='paused' THEN 1 ELSE 0 END WHERE record_id=?`, paused, paused, v.RecordID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (r *TorrentRepo) FinishStorageOperation(m *StorageMapping, v *RecoveryRecord, from string, retained []string) error {
	tx, err := r.db.BeginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkStorageFence(tx, m, v.RecordID); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE storage_recovery SET stage=?,journal=?,error='',failure_code='' WHERE hash=? AND record_id=? AND version=? AND reason=? AND stage=?`, v.Stage, v.Journal, v.Hash, v.RecordID, v.Version, v.Reason, from)
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
	if _, err := tx.Exec(`DELETE FROM storage_claims WHERE record_id=? AND operation_version=?`, v.RecordID, v.Version); err != nil {
		return err
	}
	for _, path := range retained {
		if err := payload.ValidateRelative(path); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO storage_claims(storage_id,record_id,operation_version,path,kind) VALUES(?,?,?,?,'retained')`, m.StorageID, v.RecordID, v.Version, path); err != nil {
			return err
		}
	}
	return tx.Commit()
}
