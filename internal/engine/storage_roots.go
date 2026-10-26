package engine

import (
	"context"
	"fmt"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"path/filepath"
	"torrent-stream-hub/internal/payload"
)

func (e *Engine) SetResumeMutationFactory(factory storage.FileResumeFactory) {
	e.storageRootMu.Lock()
	e.storageResumeFactory = factory
	e.storageRootMu.Unlock()
}

func (e *Engine) resumeMutationHooks(ctx context.Context, info *metainfo.Info, hash metainfo.Hash) (*storage.FileResumeHooks, error) {
	e.storageRootMu.RLock()
	factory := e.storageResumeFactory
	e.storageRootMu.RUnlock()
	if factory == nil {
		return nil, nil
	}
	return factory(ctx, info, hash)
}

func (e *Engine) SetStorageIOGuard(guard func(string) error) {
	e.storageRootMu.Lock()
	e.storageIOGuard = guard
	e.storageRootMu.Unlock()
}

func (e *Engine) checkStorageIO(path string) error {
	e.storageRootMu.RLock()
	guard := e.storageIOGuard
	e.storageRootMu.RUnlock()
	if guard != nil {
		return guard(path)
	}
	return nil
}

func (e *Engine) SetStorageLayoutResolver(resolve func(string, *metainfo.Info) (payload.Layout, error), alias func(string, string) error) {
	e.storageRootMu.Lock()
	e.storageLayoutResolver = resolve
	e.storageAliasRegistrar = alias
	e.storageRootMu.Unlock()
}

func (e *Engine) StorageLayout(hash string, info *metainfo.Info) (payload.Layout, error) {
	e.storageRootMu.RLock()
	resolve, register := e.storageLayoutResolver, e.storageAliasRegistrar
	e.storageRootMu.RUnlock()
	if resolve == nil {
		root, err := e.legacyStorageRoot(hash)
		l := payload.Layout{Version: payload.LayoutVersion, Root: root}
		if err == nil {
			err = l.Validate()
		}
		return l, err
	}
	l, err := resolve(hash, info)
	if err != nil && register != nil {
		owner := ""
		e.mu.RLock()
		for key, mt := range e.managedTorrents {
			if key != hash && mt.t != nil && mt.t.InfoHash().HexString() == hash {
				owner = key
				break
			}
		}
		e.mu.RUnlock()
		if owner != "" {
			if aliasErr := register(hash, owner); aliasErr != nil {
				return l, aliasErr
			}
			l, err = resolve(owner, info)
		}
	}
	if err != nil {
		return l, err
	}
	if err = l.Validate(); err != nil {
		return l, err
	}
	_, err = e.validateStorageRoot(l.Root)
	return l, err
}

func (e *Engine) PayloadPath(hash string, info *metainfo.Info, file *metainfo.FileInfo) (string, error) {
	l, err := e.StorageLayout(hash, info)
	if err != nil {
		return "", err
	}
	return l.File(info, file)
}

func (e *Engine) SetStorageRootResolver(resolve func(string) (string, error), aliases ...func(string, string) error) {
	e.storageRootMu.Lock()
	e.storageLayoutResolver = nil
	e.storageRootResolver = resolve
	if len(aliases) > 0 {
		e.storageAliasRegistrar = aliases[0]
	}
	e.storageRootMu.Unlock()
}

func (e *Engine) StorageRoot(hash string) (string, error) {
	l, err := e.StorageLayout(hash, nil)
	return l.Root, err
}

func (e *Engine) legacyStorageRoot(hash string) (string, error) {
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
	if payload.ValidateRelative(root) != nil {
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
