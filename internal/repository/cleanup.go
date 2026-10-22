package repository

import (
	"encoding/json"
	"fmt"
	"torrent-stream-hub/internal/models"
)

// A durable deletion manifest permits safe retries even after Drop/restart or
// when torrent metadata was received just before the engine stopped.
func (r *TorrentRepo) CleanupPaths(hash string) ([]string, bool, error) {
	var data string
	if err := r.db.DB().QueryRow(`SELECT cleanup_paths FROM torrents WHERE hash = ?`, hash).Scan(&data); err != nil {
		return nil, false, err
	}
	if data == "" {
		return nil, false, nil
	}
	var paths []string
	err := json.Unmarshal([]byte(data), &paths)
	return paths, true, err
}

func (r *TorrentRepo) SaveCleanupPaths(hash string, paths []string) error {
	return r.writeCleanupPaths(hash, paths, true)
}

// BeginCleanup records intent before Drop. A crash or a failed final manifest
// update must not turn incomplete metadata into an empty, deletable manifest.
func (r *TorrentRepo) BeginCleanup(hash string, paths []string) error {
	return r.writeCleanupPaths(hash, paths, false)
}

func (r *TorrentRepo) CleanupReady(hash string) (bool, error) {
	var ready bool
	err := r.db.DB().QueryRow(`SELECT cleanup_ready FROM torrents WHERE hash = ?`, hash).Scan(&ready)
	return ready, err
}

func (r *TorrentRepo) writeCleanupPaths(hash string, paths []string, ready bool) error {
	if paths == nil {
		paths = []string{}
	}
	data, err := json.Marshal(paths)
	if err != nil {
		return err
	}
	res, err := r.db.DB().Exec(`UPDATE torrents SET cleanup_paths = ?, cleanup_ready = ?, state = 'Paused', deletion_mode='payload', deletion_error='' WHERE hash = ? AND deletion_mode IN ('','payload')`, string(data), ready, hash)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("torrent missing or conflicting deletion mode")
	}
	return nil
}

// BeginForget uses the same admission marker as payload cleanup but never owns
// any unlink operation. The mode survives a failed final database transaction.
func (r *TorrentRepo) BeginForget(hash string) error {
	res, err := r.db.DB().Exec(`UPDATE torrents SET cleanup_paths='[]', cleanup_ready=1, state='Paused', deletion_mode='keep', deletion_error='' WHERE hash=? AND deletion_mode IN ('','keep')`, hash)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("torrent missing or conflicting deletion mode")
	}
	return nil
}

func (r *TorrentRepo) SaveDeletionError(hash string, cause error) error {
	_, err := r.db.DB().Exec(`UPDATE torrents SET deletion_error=? WHERE hash=? AND deletion_mode!=''`, cause.Error(), hash)
	return err
}

func (r *TorrentRepo) loadDeletionIntents(torrents map[string]*models.Torrent) error {
	if len(torrents) == 0 {
		return nil
	}
	rows, err := r.db.DB().Query(`SELECT hash,deletion_mode,deletion_error FROM torrents WHERE deletion_mode!=''`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var hash, mode, reason string
		if err := rows.Scan(&hash, &mode, &reason); err != nil {
			return err
		}
		if t := torrents[hash]; t != nil {
			t.DeletionMode = mode
			t.DeletionError = reason
			t.State = models.StatePaused
		}
	}
	return rows.Err()
}
