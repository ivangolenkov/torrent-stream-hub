package repository

import "torrent-stream-hub/internal/models"

// A database observation is always last-known/legacy, never proof of current
// bytes. Readiness must be established by the engine's storage verification.
func (r *TorrentRepo) loadProgressObservations(torrents map[string]*models.Torrent) error {
	if len(torrents) == 0 {
		return nil
	}
	query := `SELECT t.hash,progress_source,progress_observed_at,legacy_downloaded,piece_length,num_pieces,metadata_known,COALESCE(q.id,0) FROM torrents t LEFT JOIN torrent_order q ON q.hash=t.hash`
	var args []any
	if len(torrents) == 1 {
		for hash := range torrents {
			query += ` WHERE t.hash=?`
			args = append(args, hash)
		}
	}
	rows, err := r.db.DB().Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var hash, source string
		var observed, legacy, pieceLength int64
		var numPieces int
		var known bool
		var queuePosition int64
		if err := rows.Scan(&hash, &source, &observed, &legacy, &pieceLength, &numPieces, &known, &queuePosition); err != nil {
			return err
		}
		if t := torrents[hash]; t != nil {
			t.QueuePosition = queuePosition
			t.ProgressSource = source
			if source == "verified" {
				t.ProgressSource = "last_known"
			}
			t.ProgressObservedAt, t.LegacyDownloaded = observed, legacy
			t.MetadataKnown, t.PieceLength, t.NumPieces = known, pieceLength, numPieces
			t.Progress = models.ProgressPercent(t.Downloaded, t.Size)
		}
	}
	return rows.Err()
}
