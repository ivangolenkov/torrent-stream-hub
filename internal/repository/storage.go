package repository

import (
	"database/sql"
	"fmt"
)

type StorageMapping struct {
	Hash     string
	Version  int
	Root     string
	State    string
	Manifest string
	Error    string
}

func (r *TorrentRepo) ClaimStorageAlias(alias, owner string) error {
	_, err := r.db.DB().Exec(`INSERT INTO torrent_storage(hash,version,root,state,manifest,error) SELECT ?,version,root,state,'','' FROM torrent_storage WHERE hash=? AND state='ready' AND manifest='' ON CONFLICT(hash) DO NOTHING`, alias, owner)
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
	if a == nil || o == nil || a.Root != o.Root || a.State != "ready" {
		return fmt.Errorf("storage alias conflict")
	}
	return nil
}

func (r *TorrentRepo) StorageMapping(hash string) (*StorageMapping, error) {
	m := &StorageMapping{Hash: hash}
	err := r.db.DB().QueryRow(`SELECT version,root,state,manifest,error FROM torrent_storage WHERE hash=?`, hash).Scan(&m.Version, &m.Root, &m.State, &m.Manifest, &m.Error)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

func (r *TorrentRepo) SaveStorageMapping(m *StorageMapping) error {
	_, err := r.db.DB().Exec(`INSERT INTO torrent_storage(hash,version,root,state,manifest,error) VALUES(?,?,?,?,?,?) ON CONFLICT(hash) DO UPDATE SET version=excluded.version,root=excluded.root,state=excluded.state,manifest=excluded.manifest,error=excluded.error`, m.Hash, m.Version, m.Root, m.State, m.Manifest, m.Error)
	return err
}
