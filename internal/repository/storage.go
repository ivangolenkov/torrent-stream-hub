package repository

import (
	"database/sql"
	"fmt"
	"torrent-stream-hub/internal/payload"
)

type StorageMapping struct {
	Hash      string
	Version   int
	Root      string
	State     string
	Manifest  string
	Error     string
	StorageID string
	Revision  int64
	Prefix    string
	NameHint  string
}

func (m *StorageMapping) Layout() payload.Layout {
	return payload.Layout{Version: m.Version, Root: m.Root, Prefix: m.Prefix}
}
func (m *StorageMapping) Ready() bool {
	return m != nil && m.State == "ready" && m.Manifest == "" && m.Layout().Validate() == nil
}

func (r *TorrentRepo) ClaimStorageAlias(alias, owner string) error {
	_, err := r.db.DB().Exec(`INSERT INTO torrent_storage(hash,version,root,state,manifest,error,storage_id,revision,prefix,name_hint) SELECT ?,version,root,state,'','',storage_id,revision,prefix,name_hint FROM torrent_storage WHERE hash=? AND state IN ('ready','reserved') AND manifest='' ON CONFLICT(hash) DO NOTHING`, alias, owner)
	if err != nil {
		return err
	}
	a, err := r.StorageMapping(alias)
	if err != nil {
		return err
	}
	o, err := r.StorageMapping(owner)
	if err != nil {
		return err
	}
	if a == nil || o == nil || a.Root != o.Root || a.StorageID != o.StorageID || a.Prefix != o.Prefix || a.Revision != o.Revision || (a.State != "ready" && a.State != "reserved") {
		return fmt.Errorf("storage alias conflict")
	}
	return nil
}

func (r *TorrentRepo) StorageMapping(hash string) (*StorageMapping, error) {
	m := &StorageMapping{Hash: hash}
	err := r.db.DB().QueryRow(`SELECT version,root,state,manifest,error,storage_id,revision,prefix,name_hint FROM torrent_storage WHERE hash=?`, hash).Scan(&m.Version, &m.Root, &m.State, &m.Manifest, &m.Error, &m.StorageID, &m.Revision, &m.Prefix, &m.NameHint)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

func (r *TorrentRepo) SaveStorageMapping(m *StorageMapping) error {
	_, err := r.db.DB().Exec(`INSERT INTO torrent_storage(hash,version,root,state,manifest,error,storage_id,prefix,name_hint) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(hash) DO UPDATE SET version=excluded.version,root=excluded.root,state=excluded.state,manifest=excluded.manifest,error=excluded.error,prefix=excluded.prefix,name_hint=excluded.name_hint`, m.Hash, m.Version, m.Root, m.State, m.Manifest, m.Error, m.StorageID, m.Prefix, m.NameHint)
	return err
}

// EnsureStorageMapping initializes legacy/isolated mappings without replacing a
// reservation made by a concurrent canonical/hybrid Add or migration.
func (r *TorrentRepo) EnsureStorageMapping(m *StorageMapping) error {
	_, err := r.db.DB().Exec(`INSERT INTO torrent_storage(hash,version,root,state,manifest,error,storage_id,prefix,name_hint) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(hash) DO NOTHING`, m.Hash, m.Version, m.Root, m.State, m.Manifest, m.Error, m.StorageID, m.Prefix, m.NameHint)
	return err
}

// Alias updates may advance their own workflow, never replace another root.
func (r *TorrentRepo) SaveStorageAlias(m *StorageMapping) error {
	res, err := r.db.DB().Exec(`INSERT INTO torrent_storage(hash,version,root,state,manifest,error,storage_id,revision,prefix,name_hint) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(hash) DO UPDATE SET version=excluded.version,state=excluded.state,manifest=excluded.manifest,error=excluded.error,prefix=excluded.prefix WHERE torrent_storage.root=excluded.root AND (excluded.storage_id='' OR torrent_storage.storage_id=excluded.storage_id)`, m.Hash, m.Version, m.Root, m.State, m.Manifest, m.Error, m.StorageID, max(int64(1), m.Revision), m.Prefix, m.NameHint)
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

// StorageOwner resolves an alias to its persisted record without deriving an
// identity from a filesystem path. Ambiguous legacy ownership is a conflict.
func (r *TorrentRepo) StorageOwner(hash string) (string, error) {
	rows, err := r.db.DB().Query(`SELECT t.hash FROM torrents t WHERE t.hash=? OR t.hash IN (
 SELECT s.hash FROM torrent_storage s JOIN torrent_storage alias ON s.storage_id=alias.storage_id
 WHERE alias.hash=? AND s.storage_id!='')`, hash, hash)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var owner string
	for rows.Next() {
		var next string
		if err := rows.Scan(&next); err != nil {
			return "", err
		}
		if owner != "" && owner != next {
			return "", ErrStateConflict
		}
		owner = next
	}
	return owner, rows.Err()
}
