package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
)

func (r *TorrentRepo) InventoryRecord(ctx context.Context, hash string) (*models.Torrent, error) {
	t := &models.Torrent{Hash: hash, Control: &models.TorrentControl{}}
	err := r.db.DB().QueryRowContext(ctx, `SELECT t.metadata_known,t.deletion_mode,i.record_id FROM torrents t JOIN torrent_intents i ON i.hash=t.hash WHERE t.hash=?`, hash).Scan(&t.MetadataKnown, &t.DeletionMode, &t.Control.RecordID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

func (r *TorrentRepo) StorageVolumes() ([]payload.VolumeRegistration, error) {
	rows, err := r.db.DB().Query(`SELECT root,token,state FROM storage_volumes ORDER BY root`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []payload.VolumeRegistration
	for rows.Next() {
		var v payload.VolumeRegistration
		if err := rows.Scan(&v.Root, &v.Token, &v.State); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *TorrentRepo) BeginVolumeEnrollment(root string, freshOnly bool) (*payload.VolumeRegistration, error) {
	tx, err := r.db.BeginWrite()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if freshOnly {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM torrents`).Scan(&n); err != nil {
			return nil, err
		}
		if n != 0 {
			return nil, payload.ErrVolumeUnregistered
		}
	}
	_, err = tx.Exec(`INSERT INTO storage_volumes(root,token,state) VALUES(?,lower(hex(randomblob(16))),'enrolling') ON CONFLICT(root) DO NOTHING`, root)
	if err != nil {
		return nil, err
	}
	v := &payload.VolumeRegistration{Root: root}
	if err := tx.QueryRow(`SELECT token,state FROM storage_volumes WHERE root=?`, root).Scan(&v.Token, &v.State); err != nil {
		return nil, err
	}
	return v, tx.Commit()
}
func (r *TorrentRepo) FinishVolumeEnrollment(v payload.VolumeRegistration) error {
	res, err := r.db.DB().Exec(`UPDATE storage_volumes SET state='ready' WHERE root=? AND token=?`, v.Root, v.Token)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return ErrStateConflict
	}
	return err
}

type InventoryState struct {
	StorageID               string
	Revision                int64
	Session, Status, Reason string
	Cursor                  int
	ObservedAt              int64
}
type InventoryFile struct {
	MetadataPath     string
	Index            int
	Path             string
	Size             int64
	Expected         bool
	Presence, Reason string
	ObservedAt       int64
}

func (r *TorrentRepo) BeginInventorySession(session string) error {
	tx, err := r.db.BeginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE storage_inventory_session SET token=?`, session); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO storage_inventory(storage_id,mapping_revision,session,status)
 SELECT DISTINCT s.storage_id,s.revision,?,'pending' FROM torrent_storage s JOIN torrents t ON t.hash=s.hash WHERE s.state='ready'
 ON CONFLICT(storage_id) DO UPDATE SET mapping_revision=excluded.mapping_revision,session=excluded.session,status='pending',cursor=-1,observed_at_ms=0`, session)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (r *TorrentRepo) PrimeNewInventory(m *StorageMapping, session string) error {
	tx, err := r.db.BeginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current string
	if err := tx.QueryRow(`SELECT token FROM storage_inventory_session WHERE singleton=1`).Scan(&current); err != nil {
		return err
	}
	if current != session {
		return ErrStateConflict
	}
	_, err = tx.Exec(`INSERT INTO storage_inventory(storage_id,mapping_revision,session,status) VALUES(?,?,?,'ready') ON CONFLICT(storage_id) DO NOTHING`, m.StorageID, m.Revision, session)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (r *TorrentRepo) Inventory(id string) (*InventoryState, error) {
	v := &InventoryState{StorageID: id}
	err := r.db.DB().QueryRow(`SELECT mapping_revision,session,status,reason,cursor,observed_at_ms FROM storage_inventory WHERE storage_id=?`, id).Scan(&v.Revision, &v.Session, &v.Status, &v.Reason, &v.Cursor, &v.ObservedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return v, err
}

func (r *TorrentRepo) PendingInventories(ctx context.Context, after string, periodic bool) ([]string, error) {
	rows, err := r.db.DB().QueryContext(ctx, `SELECT t.hash FROM torrents t JOIN torrent_storage s ON s.hash=t.hash LEFT JOIN storage_inventory i ON i.storage_id=s.storage_id
 WHERE s.state='ready' AND t.deletion_mode='' AND t.hash>? AND (i.status IS NULL OR i.status IN ('pending','observing') OR (? AND i.status='ready')) ORDER BY t.hash LIMIT 64`, after, periodic)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		out = append(out, hash)
	}
	return out, rows.Err()
}

// File catalogs are read in bounded pages. The record/mapping fence is checked
// again when publishing results after stat, without a transaction across I/O.
func (r *TorrentRepo) InventoryFiles(m *StorageMapping, id string, after int) ([]InventoryFile, error) {
	rows, err := r.db.DB().Query(`SELECT f."index",f.path,f.size,COALESCE(e.expected,0)
 FROM files f LEFT JOIN file_inventory e ON e.hash=f.hash AND e.file_index=f."index" AND e.record_id=? AND e.mapping_revision=? AND e.metadata_path=f.path AND e.size=f.size
 WHERE f.hash=? AND f."index">? ORDER BY f."index" LIMIT 128`, id, m.Revision, m.Hash, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InventoryFile
	for rows.Next() {
		var v InventoryFile
		if err := rows.Scan(&v.Index, &v.Path, &v.Size, &v.Expected); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *TorrentRepo) SaveInventory(m *StorageMapping, id, session, status, reason string, cursor int, files []InventoryFile, repair bool) error {
	tx, err := r.db.BeginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkStorageFence(tx, m, id); err != nil {
		return err
	}
	var activeSession string
	if err := tx.QueryRow(`SELECT token FROM storage_inventory_session WHERE singleton=1`).Scan(&activeSession); err != nil {
		return err
	}
	if activeSession != session || session == "" {
		return ErrStateConflict
	}
	for _, f := range files {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM files WHERE hash=? AND "index"=? AND path=? AND size=?`, m.Hash, f.Index, f.Path, f.Size).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return ErrStateConflict
		}
		if f.MetadataPath != "" {
			res, err := tx.Exec(`UPDATE files SET metadata_path=? WHERE hash=? AND "index"=? AND (metadata_path='' OR metadata_path=?)`, f.MetadataPath, m.Hash, f.Index, f.MetadataPath)
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
		}
		_, err = tx.Exec(`INSERT INTO file_inventory(hash,record_id,file_index,mapping_revision,metadata_path,size,expected,presence,reason,observed_at_ms) VALUES(?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(hash,file_index) DO UPDATE SET record_id=excluded.record_id,mapping_revision=excluded.mapping_revision,metadata_path=excluded.metadata_path,size=excluded.size,expected=excluded.expected,presence=excluded.presence,reason=excluded.reason,observed_at_ms=excluded.observed_at_ms`, m.Hash, id, f.Index, m.Revision, f.Path, f.Size, f.Expected, f.Presence, f.Reason, f.ObservedAt)
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(`INSERT INTO storage_inventory(storage_id,mapping_revision,session,status,reason,cursor,observed_at_ms) VALUES(?,?,?,?,?,?,?) ON CONFLICT(storage_id) DO UPDATE SET mapping_revision=excluded.mapping_revision,session=excluded.session,status=excluded.status,reason=excluded.reason,cursor=excluded.cursor,observed_at_ms=excluded.observed_at_ms`, m.StorageID, m.Revision, session, status, reason, cursor, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	if reason == "expected_file_missing" {
		_, err = tx.Exec(`INSERT INTO torrent_activation_blocks(record_id,kind,reason) VALUES(?,'missing_files','Expected file absent during inventory') ON CONFLICT(record_id,kind) DO NOTHING`, id)
	} else if repair && status == "ready" {
		_, err = tx.Exec(`DELETE FROM torrent_activation_blocks WHERE record_id=? AND kind='missing_files' AND reason='Expected file absent during inventory'`, id)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (r *TorrentRepo) QueueInventory(m *StorageMapping, id, session string) error {
	return r.SaveInventory(m, id, session, "pending", "", -1, nil, false)
}
