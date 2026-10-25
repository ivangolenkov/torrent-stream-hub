package usecase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"
)

// The durable empty-root reservation cannot write payload. Both hashes of a
// hybrid share its ID before either can be opened by the library.
func (uc *TorrentUseCase) reserveStorageLocked(hash, alias string, names ...string) error {
	m, err := uc.repo.StorageMapping(hash)
	if err != nil {
		return err
	}
	if m == nil && alias != "" {
		a, err := uc.repo.StorageMapping(alias)
		if err != nil {
			return err
		}
		if a != nil {
			if err := uc.repo.ClaimStorageAlias(hash, alias); err != nil {
				return err
			}
			m, err = uc.repo.StorageMapping(hash)
			if err != nil {
				return err
			}
		}
	}
	if m == nil {
		hint := ""
		if len(names) > 0 && names[0] != "" {
			hint = payload.FolderName(names[0])
		}
		err = uc.repo.EnsureStorageMapping(&repository.StorageMapping{Hash: hash, Version: payload.NamedLayoutVersion, State: "reserved", NameHint: hint})
		if err != nil {
			return err
		}
		m, err = uc.repo.StorageMapping(hash)
		if err != nil {
			return err
		}
	}
	if !m.Ready() && (m == nil || m.State != "reserved" || m.Root != "" || m.Version != payload.NamedLayoutVersion) {
		return ErrStorageConflict
	}
	if m.Root != "" {
		uc.ownershipMu.Lock()
		err = uc.claimsConflictLocked(hash, []string{m.Root})
		uc.ownershipMu.Unlock()
		if err != nil {
			return err
		}
	}
	if alias != "" && alias != hash {
		return uc.claimStorageAlias(alias, hash)
	}
	return nil
}

// No filesystem work happens in the library resolver. This function runs under
// an application record reservation, before Add or in the single metadata worker.
func (uc *TorrentUseCase) prepareNamedStorage(ctx context.Context, hash string, info *metainfo.Info) error {
	if err := uc.checkVolume("."); err != nil {
		return err
	}
	m, err := uc.repo.StorageMapping(hash)
	if err != nil {
		return err
	}
	if m.Ready() {
		if err := uc.checkVolume(m.Root); err != nil {
			return err
		}
		owner, err := uc.repo.StorageOwner(hash)
		if err != nil {
			return err
		}
		if owner == "" {
			return uc.repo.PrimeNewInventory(m, uc.inventorySession)
		}
		ready, err := uc.inspectInventory(ctx, owner, false, info)
		if err != nil {
			return err
		}
		if !ready {
			return ErrInventoryPending
		}
		return nil
	}
	if m == nil || m.State != "reserved" || m.Root != "" || m.Version != payload.NamedLayoutVersion {
		return ErrStorageConflict
	}
	id := ""
	owner, err := uc.repo.StorageOwner(hash)
	if err != nil {
		return err
	}
	if owner != "" {
		m, err = uc.repo.StorageMapping(owner)
		if err != nil {
			return err
		}
		i, err := uc.repo.GetIntent(owner)
		if err != nil {
			return err
		}
		if i == nil {
			return ErrStorageConflict
		}
		id = i.Version.RecordID
		hash = owner
	}
	root, err := os.OpenRoot(uc.engine.DownloadDir())
	if err != nil {
		return err
	}
	defer root.Close()
	name := m.NameHint
	if name == "" {
		name = info.BestName()
	}
	for ordinal := 1; ordinal <= 4096; ordinal++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		next := payload.Layout{Version: payload.NamedLayoutVersion, Root: payload.NamedRoot(name, ordinal), Prefix: payload.MetadataPrefix(info)}
		for _, f := range info.UpvertedFiles() {
			if _, err := next.File(info, &f); err != nil {
				return err
			}
		}
		exists, err := namedPathOccupied(root, next.Root)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		release, err := uc.reserveOwnership(ctx, root, hash, []string{next.Root})
		if errors.Is(err, ErrStorageConflict) {
			continue
		}
		if err != nil {
			return err
		}
		uc.ownershipMu.Lock()
		err = uc.repo.SetStorageLayout(ctx, m, id, next)
		uc.ownershipMu.Unlock()
		release()
		if err != nil {
			return err
		}
		current, err := uc.repo.StorageMapping(hash)
		if err != nil {
			return err
		}
		return uc.repo.PrimeNewInventory(current, uc.inventorySession)
	}
	return fmt.Errorf("%w: no available folder name", ErrStorageConflict)
}

// Compare names portably even on a case-sensitive filesystem. Enumeration uses
// bounded batches; an unknown directory is occupied, never adopted by Add.
func namedPathOccupied(root *os.Root, path string) (bool, error) {
	dir, err := root.Open(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer dir.Close()
	key := payload.CollisionKey(filepath.Base(path))
	for {
		entries, err := dir.ReadDir(128)
		for _, entry := range entries {
			if payload.CollisionKey(entry.Name()) == key {
				return true, nil
			}
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

func (uc *TorrentUseCase) runLayoutReservations(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	cursor := ""
	process := func(hash string) {
		owner, err := uc.repo.StorageOwner(hash)
		if err != nil || owner == "" {
			return
		}
		release, err := uc.tryLockRecord(owner)
		if err != nil {
			return
		}
		defer release()
		t, err := uc.repo.GetTorrent(owner)
		if err != nil || t == nil || t.DeletionMode != "" {
			return
		}
		if err := uc.checkStorageWorkflow(owner); err != nil {
			return
		}
		h := uc.engine.StorageMetadata(owner)
		mi := h.Pending()
		if mi == nil {
			return
		}
		info, err := mi.UnmarshalInfo()
		if err == nil {
			err = uc.prepareNamedStorage(ctx, owner, &info)
		}
		if err == nil {
			err = h.RetryStorage(mi)
		}
		if err == nil {
			if m, e := uc.repo.StorageMapping(owner); e == nil && m != nil && m.Error != "" {
				_, _ = uc.repo.SetStoragePreparationError(m, t.Control.RecordID, "")
			}
		}
		if err != nil && ctx.Err() == nil {
			m, readErr := uc.repo.StorageMapping(owner)
			if readErr == nil && m != nil {
				changed, saveErr := uc.repo.SetStoragePreparationError(m, t.Control.RecordID, err.Error())
				if changed || saveErr != nil {
					logging.Warnf("storage preparation hash=%s: %v (diagnostic: %v)", owner, err, saveErr)
				}
			}
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case hash := <-uc.layoutWake:
			process(hash)
		case <-tick.C:
			hashes, err := uc.repo.PendingStorageReservations(ctx, cursor, 64)
			if err != nil {
				continue
			}
			for _, hash := range hashes {
				if ctx.Err() != nil {
					return
				}
				process(hash)
				cursor = hash
			}
			if len(hashes) < 64 {
				cursor = ""
			}
		}
	}
}
