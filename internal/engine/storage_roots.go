package engine

import (
	"fmt"
	"path/filepath"
	"torrent-stream-hub/internal/payload"
)

func (e *Engine) SetStorageRootResolver(resolve func(string) (string, error), aliases ...func(string, string) error) {
	e.storageRootMu.Lock()
	e.storageRootResolver = resolve
	if len(aliases) > 0 {
		e.storageAliasRegistrar = aliases[0]
	}
	e.storageRootMu.Unlock()
}

func (e *Engine) StorageRoot(hash string) (string, error) {
	e.storageRootMu.RLock()
	resolve, register := e.storageRootResolver, e.storageAliasRegistrar
	e.storageRootMu.RUnlock()
	if resolve != nil {
		root, err := resolve(hash)
		if err == nil {
			return e.validateStorageRoot(root)
		}
		if register == nil {
			return root, err
		}
		// Storage is being opened under the torrent client lock after a hybrid
		// magnet acquired its other hash. Reuse the already admitted runtime's
		// stable identity, never allocate a second payload root.
		owner := ""
		e.mu.RLock()
		for key, mt := range e.managedTorrents {
			if mt.t != nil && key != hash && mt.t.InfoHash().HexString() == hash {
				owner = key
				break
			}
		}
		e.mu.RUnlock()
		if owner == "" {
			return "", err
		}
		root, err = resolve(owner)
		if err != nil {
			return "", err
		}
		if err := register(hash, owner); err != nil {
			return "", err
		}
		return e.validateStorageRoot(root)
	}
	return e.validateStorageRoot(payload.Root(hash))
}

func (e *Engine) validateStorageRoot(root string) (string, error) {
	if !payload.ValidRoot(root) {
		return "", fmt.Errorf("invalid isolated storage mapping")
	}
	abs, err := filepath.Abs(filepath.Join(e.cfg.DownloadDir, root))
	if err != nil {
		return "", err
	}
	for _, protected := range e.CleanupProtectedPaths() {
		other, err := filepath.Abs(protected)
		if err != nil {
			return "", err
		}
		if payload.Overlap(abs, other) {
			return "", fmt.Errorf("isolated payload overlaps service storage")
		}
	}
	return root, nil
}
