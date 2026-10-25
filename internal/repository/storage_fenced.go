package repository

// A late migration error/checkpoint cannot mutate a re-added hash's mapping.
func (r *TorrentRepo) SaveStorageMappingForRecord(m *StorageMapping, recordID string) error {
	res, err := r.db.DB().Exec(`INSERT INTO torrent_storage(hash,version,root,state,manifest,error)
 SELECT ?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM torrent_intents WHERE hash=? AND record_id=?)
 ON CONFLICT(hash) DO UPDATE SET version=excluded.version,root=excluded.root,state=excluded.state,manifest=excluded.manifest,error=excluded.error`, m.Hash, m.Version, m.Root, m.State, m.Manifest, m.Error, m.Hash, recordID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return ErrStateConflict
	}
	return err
}
