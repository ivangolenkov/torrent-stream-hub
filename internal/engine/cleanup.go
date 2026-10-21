package engine

import "path/filepath"

// CleanupProtectedPaths lists application-owned storage which must never be
// treated as torrent payload, even if untrusted metainfo claims those names.
func (e *Engine) CleanupProtectedPaths() []string {
	paths := []string{
		filepath.Join(e.cfg.DownloadDir, ".torrent.bolt.db"),
		filepath.Join(e.ConfigDir(), "metainfo"),
	}
	if e.cfg.DBPath != "" {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			paths = append(paths, e.cfg.DBPath+suffix)
		}
	}
	if e.cfg.LogDir != "" {
		paths = append(paths, e.cfg.LogDir)
	}
	if e.cfg.BTRetrackersFile != "" {
		paths = append(paths, e.cfg.BTRetrackersFile)
	}
	return paths
}
