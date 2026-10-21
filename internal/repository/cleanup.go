package repository

import "encoding/json"

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
	_, err = r.db.DB().Exec(`UPDATE torrents SET cleanup_paths = ?, cleanup_ready = ?, state = 'Paused' WHERE hash = ?`, string(data), ready, hash)
	return err
}
