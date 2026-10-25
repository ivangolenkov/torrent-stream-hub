package repository

import "context"

func (r *TorrentRepo) PendingStorageReservations(ctx context.Context, after string, limit int) ([]string, error) {
	rows, err := r.db.DB().QueryContext(ctx, `SELECT s.hash FROM torrent_storage s JOIN torrents t ON t.hash=s.hash WHERE s.state IN ('reserved','ready') AND t.metadata_known=0 AND s.hash>? ORDER BY s.hash LIMIT ?`, after, min(64, max(1, limit)))
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

// Preparation errors are visible without a write/log on every polling tick.
// The worker cannot publish a result to a deleted/re-added record or moved root.
func (r *TorrentRepo) SetStoragePreparationError(m *StorageMapping, recordID, message string) (bool, error) {
	res, err := r.db.DB().Exec(`UPDATE torrent_storage SET error=? WHERE storage_id=? AND revision=? AND error!=? AND EXISTS(SELECT 1 FROM torrent_intents WHERE hash=? AND record_id=?)`, message, m.StorageID, m.Revision, message, m.Hash, recordID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
