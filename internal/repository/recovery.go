package repository

import (
	"database/sql"
	"encoding/json"
	"errors"
	"torrent-stream-hub/internal/models"
)

func (r *TorrentRepo) NextRecoveryVersion(v *RecoveryRecord) error {
	res, err := r.db.DB().Exec(`UPDATE storage_recovery SET version=version+1 WHERE hash=? AND record_id=? AND version=? AND version<9223372036854775807`, v.Hash, v.RecordID, v.Version)
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
	v.Version++
	return nil
}

var ErrDeletionCancelled = errors.New("deletion cancellation requested")

type RecoveryRecord struct {
	Hash, RecordID                     string
	Version                            int64
	Reason                             models.StorageRecoveryReason
	Stage, RemovedData, Error, Journal string
	FailureCode                        string
	Mode                               string
}

func (r *TorrentRepo) Recovery(hash string) (*RecoveryRecord, error) {
	v := &RecoveryRecord{Hash: hash}
	err := r.db.DB().QueryRow(`SELECT record_id,version,reason,stage,removed_data,error,journal,failure_code,mode FROM storage_recovery WHERE hash=?`, hash).Scan(&v.RecordID, &v.Version, &v.Reason, &v.Stage, &v.RemovedData, &v.Error, &v.Journal, &v.FailureCode, &v.Mode)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return v, err
}
func (r *TorrentRepo) SaveRecovery(v *RecoveryRecord) error {
	res, err := r.db.DB().Exec(`INSERT INTO storage_recovery(hash,record_id,version,reason,stage,removed_data,error,journal,failure_code)
 SELECT ?,?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM torrent_intents WHERE hash=? AND record_id=?)
 ON CONFLICT(hash) DO UPDATE SET reason=excluded.reason,stage=excluded.stage,removed_data=excluded.removed_data,error=excluded.error,journal=excluded.journal,failure_code=excluded.failure_code
 WHERE storage_recovery.record_id=excluded.record_id AND storage_recovery.version=excluded.version`, v.Hash, v.RecordID, v.Version, v.Reason, v.Stage, v.RemovedData, v.Error, v.Journal, v.FailureCode, v.Hash, v.RecordID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return ErrStateConflict
	}
	return err
}
func (r *TorrentRepo) ClearRecovery(hash, id string, version int64) error {
	res, err := r.db.DB().Exec(`DELETE FROM storage_recovery WHERE hash=? AND record_id=? AND version=?`, hash, id, version)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return ErrStateConflict
	}
	return err
}
func (r *TorrentRepo) DeletionGate(hash, id string) error {
	var stage string
	err := r.db.DB().QueryRow(`SELECT stage FROM storage_recovery WHERE hash=? AND record_id=? AND reason='deletion_pending' AND EXISTS(SELECT 1 FROM torrent_intents WHERE hash=? AND record_id=?)`, hash, id, hash, id).Scan(&stage)
	if err == sql.ErrNoRows {
		return ErrDeletionCancelled
	}
	if err != nil {
		return err
	}
	if stage == "cancel_requested" || stage == "cancelled" {
		return ErrDeletionCancelled
	}
	return nil
}

// The cancellation marker is committed before waiting for the owning worker.
// Intent is durably paused in the same short transaction, even on disconnect.
func (r *TorrentRepo) RequestDeletionCancel(hash, id string, version int64) error {
	tx, err := r.db.BeginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE storage_recovery SET stage=CASE WHEN stage='cancelled' THEN stage ELSE 'cancel_requested' END WHERE hash=? AND record_id=? AND version=? AND (reason='deletion_pending' OR (reason='deletion_cancelled' AND stage='cancelled'))`, hash, id, version)
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
	_, err = tx.Exec(`UPDATE torrent_intents SET run_intent='paused',revision=revision+CASE WHEN run_intent='paused' THEN 0 ELSE 1 END WHERE hash=? AND record_id=?`, hash, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (r *TorrentRepo) CompleteDeletionCancel(hash, id string, version int64) error {
	tx, err := r.db.BeginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE storage_recovery SET reason='deletion_cancelled',stage='cancelled',error='' WHERE hash=? AND record_id=? AND version=? AND stage IN ('cancel_requested','cancelled')`, hash, id, version)
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
	_, err = tx.Exec(`UPDATE torrents SET deletion_mode='',deletion_error='',cleanup_paths='',cleanup_ready=0,state='Paused' WHERE hash=? AND EXISTS(SELECT 1 FROM torrent_intents WHERE hash=? AND record_id=?)`, hash, hash, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (r *TorrentRepo) AdvanceDeletion(hash, id, stage, removed string) error {
	res, err := r.db.DB().Exec(`UPDATE storage_recovery SET stage=?,removed_data=CASE WHEN ?='' THEN removed_data ELSE ? END WHERE hash=? AND record_id=? AND reason='deletion_pending' AND stage NOT IN ('cancel_requested','cancelled')`, stage, removed, removed, hash, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return ErrDeletionCancelled
	}
	return err
}

func (r *TorrentRepo) MarkDeletionPartial(hash, id string) error {
	_, err := r.db.DB().Exec(`UPDATE storage_recovery SET removed_data='partial' WHERE hash=? AND record_id=? AND reason='deletion_pending'`, hash, id)
	return err
}

func loadStorageRecovery(tx sqlReader, torrents map[string]*models.Torrent) error {
	rows, err := tx.Query(`SELECT hash,record_id,version,reason,stage,removed_data,error,journal,failure_code,mode FROM storage_recovery`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var hash, stage, removed, message, journal, mode string
		s := &models.StorageRecovery{Runtime: "absent", DataReadiness: "unknown", Actions: []models.StorageRecoveryAction{}}
		if err := rows.Scan(&hash, &s.RecordID, &s.Version, &s.Reason, &stage, &removed, &message, &journal, &s.FailureCode, &mode); err != nil {
			return err
		}
		t := torrents[hash]
		if t == nil || t.Control == nil || t.Control.RecordID != s.RecordID {
			continue
		}
		switch s.Reason {
		case models.StorageDeletionPending:
			s.Actions = []models.StorageRecoveryAction{models.StorageRetryDeletion, models.StorageCancelDeletion}
			s.Deletion = &models.StorageDeletionStatus{Mode: t.DeletionMode, Stage: stage, RemovedData: removed, Error: t.DeletionError}
		case models.StorageDeletionCancelled:
			s.Actions = []models.StorageRecoveryAction{models.StorageForgetRecord, models.StorageResumeVerify}
			s.Deletion = &models.StorageDeletionStatus{Mode: mode, Stage: stage, RemovedData: removed, Error: message}
			if stage == "verifying" {
				s.Actions = []models.StorageRecoveryAction{models.StorageResumeVerify}
			}
		case models.StorageLegacyConflict:
			s.Runtime = "failed"
			// This typed importer diagnosis is issued only after hash-matching
			// metainfo was loaded. A missing runtime is not metadata-pending.
			t.MetadataKnown = true
			s.Actions = []models.StorageRecoveryAction{models.StorageAnalyzeLegacy}
			if stage == "analyzed" || stage == "prepared" || stage == "preserved" || stage == "published" || stage == "verified" {
				s.Actions = append(s.Actions, models.StorageApplyLegacy)
			}
			if stage == "prepared" || stage == "preserved" || stage == "published" {
				s.Actions = []models.StorageRecoveryAction{models.StorageApplyLegacy}
				if stage == "prepared" && s.FailureCode == "candidate_changed" {
					s.Actions = append(s.Actions, models.StorageAnalyzeLegacy)
				}
			}
		case models.StorageRecoveryComplete:
			s.Actions = []models.StorageRecoveryAction{models.StorageResumeVerify}
		case models.StorageAttachmentFailed:
			s.Runtime = "failed"
			s.Actions = []models.StorageRecoveryAction{models.StorageResumeVerify}
		case models.StorageCatalogChanged:
			s.Actions = []models.StorageRecoveryAction{models.StorageRetryOwnership}
		}
		if journal != "" {
			var detail struct {
				MetadataPath string                 `json:"metadata_path"`
				Analysis     *models.LegacyAnalysis `json:"analysis"`
				Alternatives []struct {
					Size int64 `json:"size"`
				} `json:"alternatives"`
			}
			if err := json.Unmarshal([]byte(journal), &detail); err != nil {
				return err
			}
			if s.Reason == models.StorageLegacyConflict || s.Reason == models.StorageRecoveryComplete {
				s.MetadataPath = detail.MetadataPath
				s.Analysis = detail.Analysis
			}
			s.AlternativeCopies = len(detail.Alternatives)
			for _, a := range detail.Alternatives {
				s.AlternativeBytes += a.Size
			}
		}
		if message != "" {
			t.StorageError = message
		}
		t.StorageRecovery = s
	}
	return rows.Err()
}
