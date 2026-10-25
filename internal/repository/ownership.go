package repository

import (
	"context"
	"encoding/json"
	"torrent-stream-hub/internal/models"
)

type OwnershipRecord struct {
	Torrent      *models.Torrent
	Mapping      *StorageMapping
	Cleanup      []string
	CleanupReady bool
	Pending      bool
}
type OwnershipSnapshot struct {
	Version  int64
	Records  []OwnershipRecord
	Mappings map[string]*StorageMapping
	Claims   []StorageClaim
}

func (r *TorrentRepo) OwnershipVersion(ctx context.Context) (int64, error) {
	var v int64
	err := r.db.DB().QueryRowContext(ctx, `SELECT version FROM storage_catalog_version`).Scan(&v)
	return v, err
}

// All ownership/identity/mapping/manifest rows come from one short WAL snapshot.
// No metainfo or filesystem I/O is performed while the transaction is open.
func (r *TorrentRepo) ReadOwnership(ctx context.Context) (*OwnershipSnapshot, error) {
	raw, err := r.db.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer raw.Rollback()
	tx := contextReadTx{Tx: raw, ctx: ctx}
	s := &OwnershipSnapshot{Mappings: make(map[string]*StorageMapping)}
	if err := tx.QueryRow(`SELECT version FROM storage_catalog_version`).Scan(&s.Version); err != nil {
		return nil, err
	}
	all, err := r.allTorrents(tx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(`SELECT hash,version,root,state,manifest,error,storage_id,revision,prefix,name_hint FROM torrent_storage`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		m := new(StorageMapping)
		if err := rows.Scan(&m.Hash, &m.Version, &m.Root, &m.State, &m.Manifest, &m.Error, &m.StorageID, &m.Revision, &m.Prefix, &m.NameHint); err != nil {
			rows.Close()
			return nil, err
		}
		s.Mappings[m.Hash] = m
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = tx.Query(`SELECT storage_id,record_id,operation_version,path,kind FROM storage_claims`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c StorageClaim
		if err := rows.Scan(&c.StorageID, &c.RecordID, &c.Version, &c.Path, &c.Kind); err != nil {
			rows.Close()
			return nil, err
		}
		s.Claims = append(s.Claims, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, t := range all {
		rec := OwnershipRecord{Torrent: t, Mapping: s.Mappings[t.Hash]}
		var data string
		if err := tx.QueryRow(`SELECT cleanup_paths,cleanup_ready FROM torrents WHERE hash=?`, t.Hash).Scan(&data, &rec.CleanupReady); err != nil {
			return nil, err
		}
		rec.Pending = data != ""
		if rec.Pending {
			if err := json.Unmarshal([]byte(data), &rec.Cleanup); err != nil {
				return nil, err
			}
		}
		s.Records = append(s.Records, rec)
	}
	return s, raw.Commit()
}
