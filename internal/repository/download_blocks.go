package repository

import (
	"torrent-stream-hub/internal/models"
)

func (r *TorrentRepo) SaveDownloadBlock(hash, reason string) error {
	if reason == "" {
		_, err := r.db.DB().Exec(`DELETE FROM torrent_download_blocks WHERE hash=?`, hash)
		return err
	}
	_, err := r.db.DB().Exec(`INSERT INTO torrent_download_blocks(hash,reason) VALUES (?,?) ON CONFLICT(hash) DO UPDATE SET reason=excluded.reason`, hash, reason)
	return err
}

func (r *TorrentRepo) DownloadBlock(hash string) (string, error) {
	var reason string
	err := r.db.DB().QueryRow(`SELECT COALESCE((SELECT reason FROM torrent_download_blocks WHERE hash=?),'')`, hash).Scan(&reason)
	return reason, err
}

func (r *TorrentRepo) loadDownloadBlocks(tx sqlReader, torrents map[string]*models.Torrent) error {
	if len(torrents) == 0 {
		return nil
	}
	rows, err := tx.Query(`SELECT hash,reason FROM torrent_download_blocks`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var hash, reason string
		if err := rows.Scan(&hash, &reason); err != nil {
			return err
		}
		if t := torrents[hash]; t != nil {
			t.DownloadBlock = reason
		}
	}
	return rows.Err()
}
