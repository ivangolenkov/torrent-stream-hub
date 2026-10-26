package usecase

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

func storagePaths(info *metainfo.Info) ([]string, error) {
	var paths []string
	for _, file := range info.UpvertedFiles() {
		path := payload.Path(info, &file)
		if err := validateRelativeDownloadPath(path); err != nil {
			return nil, fmt.Errorf("%w: invalid storage layout", ErrStorageConflict)
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func (uc *TorrentUseCase) knownStoragePaths(t *models.Torrent) ([]string, error) {
	if paths, pending, err := uc.repo.CleanupPaths(t.Hash); err != nil || pending {
		if err != nil {
			return nil, err
		}
		ready, err := uc.repo.CleanupReady(t.Hash)
		if err != nil || ready {
			return paths, err
		}
	}
	m, err := uc.repo.StorageMapping(t.Hash)
	if err != nil {
		return nil, err
	}
	if err := checkMappingVersion(m); err != nil {
		return nil, err
	}
	if m != nil && payload.Supported(m.Version) {
		paths, known, err := uc.metadataStoragePaths(t)
		if err != nil {
			return nil, err
		}
		if !known {
			if m.Root == "" {
				return nil, nil
			}
			return []string{m.Root}, nil
		}
		return paths, nil
	}
	paths, _, err := uc.metadataStoragePaths(t)
	return paths, err
}

func (uc *TorrentUseCase) metadataStoragePaths(t *models.Torrent) ([]string, bool, error) {
	info, err := uc.metadataStorageInfo(t)
	if err != nil || info == nil {
		return nil, false, err
	}
	paths, err := storagePaths(info)
	if err != nil {
		return nil, false, err
	}
	m, err := uc.repo.StorageMapping(t.Hash)
	if err != nil {
		return nil, false, err
	}
	if err := checkMappingVersion(m); err != nil {
		return nil, false, err
	}
	if m != nil && m.State == "reserved" && m.Root == "" {
		return nil, false, nil
	}
	if m != nil && payload.Supported(m.Version) {
		for i, path := range paths {
			paths[i], err = m.Layout().Path(path)
			if err != nil {
				return nil, false, err
			}
		}
	}
	return paths, true, err
}

func (uc *TorrentUseCase) metadataStorageInfo(t *models.Torrent) (*metainfo.Info, error) {
	if info, err := uc.engine.StorageMetadata(t.Hash).Info(); err != nil || info != nil {
		return info, err
	}
	mi, err := metainfo.LoadFromFile(filepath.Join(uc.engine.ConfigDir(), "metainfo", strings.ToLower(t.Hash)+".torrent"))
	if err == nil {
		spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
		if err != nil {
			return nil, err
		}
		matches := spec.InfoHash.HexString() == t.Hash
		if spec.InfoHashV2.Ok {
			v2 := spec.InfoHashV2.Value
			matches = matches || v2.ToShort().HexString() == t.Hash
		}
		if !matches {
			return nil, fmt.Errorf("%w: metainfo hash mismatch", ErrStorageConflict)
		}
		info, err := mi.UnmarshalInfo()
		if err != nil {
			return nil, err
		}
		return &info, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(t.Files) != 0 {
		return nil, fmt.Errorf("%w: cannot prove storage layout without metainfo", ErrStorageConflict)
	}
	return nil, nil // No metadata and therefore no known data files.
}

func (uc *TorrentUseCase) deleteTemporaryLocked(hash string) error {
	t, err := uc.repo.GetTorrent(hash)
	if err != nil {
		return err
	}
	if t == nil || !t.IsTemporary {
		return fmt.Errorf("cleanup candidate is no longer temporary")
	}
	return uc.deletePayloadLocked(t)
}

// Caller owns record/root handles. Manual deletion and FIFO share this workflow.
func (uc *TorrentUseCase) deletePayloadLocked(t *models.Torrent) (retErr error) {
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, uc.repo.SaveDeletionError(t.Hash, retErr))
		}
	}()
	if t.DeletionMode == "keep" {
		return ErrStorageConflict
	}
	hash := t.Hash
	if recovery, err := uc.repo.Recovery(hash); err != nil {
		return err
	} else if recoveryBlocks(recovery) {
		return ErrStorageConflict
	}
	if m, err := uc.repo.StorageMapping(hash); err != nil {
		return err
	} else if m != nil && (m.State == "moving" || m.Manifest != "") {
		return ErrStorageConflict
	}
	if err := uc.revokeResumeMutation(uc.engine.Context(), hash); err != nil {
		return err
	}
	paths, pending, err := uc.repo.CleanupPaths(hash)
	if err != nil {
		return err
	}
	ready, err := uc.repo.CleanupReady(hash)
	if err != nil {
		return err
	}
	if !pending || !ready {
		// Drop first, then inspect frozen metadata: metadata may arrive during Drop.
		raw := uc.engine.StorageMetadata(hash)
		var known bool
		paths, known, err = uc.metadataStoragePaths(t)
		if err != nil {
			return err
		}
		if pending && !known {
			return fmt.Errorf("%w: incomplete cleanup manifest requires metadata before retry", ErrStorageConflict)
		}
		if !known && !raw.Exists() {
			return fmt.Errorf("%w: cannot prove storage layout", ErrStorageConflict)
		}
		if err := uc.repo.BeginCleanup(hash, paths); err != nil {
			return err
		}
		if uc.beforeDetach != nil {
			uc.beforeDetach(hash)
		}
		if err := uc.engine.Delete(hash); err != nil {
			return err
		}
		if err := uc.repo.AdvanceDeletion(hash, t.Control.RecordID, "runtime_stopped", ""); err != nil {
			return err
		}
		if info, infoErr := raw.Info(); infoErr != nil {
			return infoErr
		} else if info != nil {
			paths, err = storagePaths(info)
			if m, e := uc.repo.StorageMapping(hash); e != nil {
				return e
			} else if e = checkMappingVersion(m); e != nil {
				return e
			} else if m != nil && payload.Supported(m.Version) {
				for i, path := range paths {
					paths[i], err = m.Layout().Path(path)
					if err != nil {
						return err
					}
				}
			}
			if err != nil {
				return err
			}
		}
		// Persist before the first unlink, so partial deletion is recoverable.
		if err := uc.repo.SaveCleanupPaths(hash, paths); err != nil {
			return err
		}
	} else {
		// Idempotently stop any remaining runtime before retrying the manifest.
		if uc.beforeDetach != nil {
			uc.beforeDetach(hash)
		}
		if err := uc.engine.Delete(hash); err != nil {
			return err
		}
		if err := uc.repo.AdvanceDeletion(hash, t.Control.RecordID, "runtime_stopped", ""); err != nil {
			return err
		}
	}
	if err := uc.repo.AdvanceDeletion(hash, t.Control.RecordID, "manifest_ready", ""); err != nil {
		return err
	}
	var owned []string
	for _, p := range paths {
		owned = append(owned, p, p+".part")
	}
	root, err := os.OpenRoot(uc.engine.DownloadDir())
	if err != nil {
		return err
	}
	defer root.Close()
	ownershipRelease, err := uc.reserveOwnership(uc.engine.Context(), root, hash, owned)
	if err != nil {
		return err
	}
	defer ownershipRelease()
	// Validate the entire manifest before removing any entry.
	for _, path := range owned {
		if err := uc.checkVolume(path); err != nil {
			return err
		}
		if err := validateCleanupPath(root, path); err != nil {
			return fmt.Errorf("%w: %v", ErrStorageConflict, err)
		}
	}
	remove := uc.payloadRemove
	if remove == nil {
		remove = func(root *os.Root, path string) error { return root.Remove(path) }
	}
	if err := uc.repo.AdvanceDeletion(hash, t.Control.RecordID, "removing", "unknown"); err != nil {
		return err
	}
	removedAny := false
	for _, path := range owned {
		if err := uc.engine.Context().Err(); err != nil {
			return err
		}
		if err := uc.repo.DeletionGate(hash, t.Control.RecordID); err != nil {
			return err
		}
		if err := uc.checkVolume(path); err != nil {
			return err
		}
		removeErr := remove(root, path)
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return removeErr
		}
		if removeErr == nil && !removedAny {
			removedAny = true
			if err := uc.repo.MarkDeletionPartial(hash, t.Control.RecordID); err != nil {
				return err
			}
		}
		for dir := filepath.Dir(path); dir != "."; dir = filepath.Dir(dir) {
			if err := root.Remove(dir); err != nil {
				break
			} // Empty directories only.
		}
	}
	if err := uc.repo.AdvanceDeletion(hash, t.Control.RecordID, "removing_record", ""); err != nil {
		return err
	}
	return uc.repo.DeleteTorrent(hash)
}

func checkMappingVersion(m *repository.StorageMapping) error {
	if m == nil || m.Version == 0 {
		return nil
	} // explicit shared-layout adapter
	if m.Version == payload.NamedLayoutVersion && m.State == "reserved" && m.Root == "" {
		return nil
	}
	if err := m.Layout().Validate(); err != nil {
		return fmt.Errorf("%w: unsupported or invalid storage layout: %v", ErrStorageConflict, err)
	}
	return nil
}

func (uc *TorrentUseCase) validateOwnership(root *os.Root, hash string, owned []string, all []*models.Torrent) error {
	return uc.validateOwnershipContext(uc.engine.Context(), root, hash, owned, all)
}

// Cache only within one ownership check. Large catalogs share ancestors and
// compare the same files many times; none of these observations authorize a
// later open, which still goes through payload's rooted validation.
func cachedStorageStat(stat func(string) (os.FileInfo, error)) func(string) (os.FileInfo, error) {
	type result struct {
		info os.FileInfo
		err  error
	}
	cache := make(map[string]result)
	return func(path string) (os.FileInfo, error) {
		if r, ok := cache[path]; ok {
			return r.info, r.err
		}
		info, err := stat(path)
		cache[path] = result{info, err}
		return info, err
	}
}

func (uc *TorrentUseCase) validateOwnershipContext(ctx context.Context, root *os.Root, hash string, owned []string, all []*models.Torrent) error {
	release, err := uc.reserveOwnership(ctx, root, hash, owned)
	if err == nil {
		release()
	}
	return err
}

func (uc *TorrentUseCase) validateOwnershipSnapshot(ctx context.Context, root *os.Root, hash string, owned []string, snapshot *repository.OwnershipSnapshot) error {
	for _, c := range snapshot.Claims {
		if m := snapshot.Mappings[hash]; m != nil && c.StorageID == m.StorageID {
			continue
		}
		for _, p := range owned {
			if ownershipOverlap(p, c.Path) {
				return fmt.Errorf("%w: durable storage reservation", ErrStorageConflict)
			}
		}
	}
	stat := cachedStorageStat(root.Stat)
	protectedStat := cachedStorageStat(os.Stat)
	var protectedPaths []string
	for _, protected := range uc.engine.CleanupProtectedPaths() {
		reserved, err := filepath.Abs(protected)
		if err != nil {
			return err
		}
		protectedPaths = append(protectedPaths, reserved)
	}
	for _, path := range owned {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := validateRelativeDownloadPath(path); err != nil {
			return fmt.Errorf("%w: invalid manifest", ErrStorageConflict)
		}
		absolute, err := filepath.Abs(filepath.Join(uc.engine.DownloadDir(), path))
		if err != nil {
			return err
		}
		for _, reserved := range protectedPaths {
			if pathsOverlap(absolute, reserved) {
				return fmt.Errorf("%w: torrent path overlaps application storage", ErrStorageConflict)
			}
			// SameFile also detects case/unicode aliases on macOS/Windows,
			// including a payload nested in an aliased application directory.
			if reservedInfo, err := protectedStat(reserved); err == nil {
				for part := path; part != "."; part = filepath.Dir(part) {
					if info, err := stat(part); err == nil && os.SameFile(info, reservedInfo) {
						return fmt.Errorf("%w: torrent path aliases application storage", ErrStorageConflict)
					}
				}
			}
		}
	}
	for _, rec := range snapshot.Records {
		other := rec.Torrent
		if other.Hash == hash || (rec.Mapping != nil && rec.Mapping.State == "reserved" && rec.Mapping.Root == "") {
			continue
		}
		others, err := uc.snapshotStoragePaths(rec)
		if err != nil {
			return fmt.Errorf("cannot verify other torrent %s: %w", other.Hash, err)
		}
		if len(owned) > 0 && len(others) == 0 {
			return fmt.Errorf("%w: cannot prove non-overlap with metadata-pending torrent %s", ErrStorageConflict, other.Hash)
		}
		for _, p := range others {
			for _, q := range []string{p, p + ".part"} {
				for _, own := range owned {
					if err := ctx.Err(); err != nil {
						return err
					}
					if pathsOverlap(own, q) {
						return fmt.Errorf("%w: shared storage path with torrent %s", ErrStorageConflict, other.Hash)
					}
					alias, err := storagePathsAlias(stat, own, q)
					if err != nil {
						return err
					}
					if alias {
						return fmt.Errorf("%w: aliased storage path with torrent %s", ErrStorageConflict, other.Hash)
					}
				}
			}
		}
	}
	// Orphan mappings are intentional keep-data namespace reservations, and
	// aliases can exist before the canonical torrent record is committed.
	for alias, m := range snapshot.Mappings {
		if alias == hash || (m.State == "reserved" && m.Root == "") {
			continue
		}
		if m.Root == "" {
			represented := false
			for _, rec := range snapshot.Records {
				if rec.Torrent.Hash == alias {
					represented = true
					break
				}
			}
			if represented {
				continue
			}
			info, err := uc.metadataStorageInfo(&models.Torrent{Hash: alias})
			if err != nil {
				return err
			}
			if info == nil {
				return fmt.Errorf("%w: kept legacy namespace requires metainfo", ErrStorageConflict)
			}
			paths, err := storagePaths(info)
			if err != nil {
				return err
			}
			for _, p := range paths {
				for _, q := range []string{p, p + ".part"} {
					for _, own := range owned {
						if ownershipOverlap(own, q) {
							return fmt.Errorf("%w: kept legacy payload reservation", ErrStorageConflict)
						}
						same, err := storagePathsAlias(stat, own, q)
						if err != nil {
							return err
						}
						if same {
							return fmt.Errorf("%w: kept legacy payload alias", ErrStorageConflict)
						}
					}
				}
			}
			continue
		}
		if own := snapshot.Mappings[hash]; own != nil && own.Root == m.Root {
			continue
		}
		for _, p := range owned {
			if ownershipOverlap(p, m.Root) {
				return fmt.Errorf("%w: reserved storage namespace", ErrStorageConflict)
			}
		}
	}
	return nil
}

func storagePathsAlias(stat func(string) (os.FileInfo, error), a, b string) (bool, error) {
	left, err := stat(a)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	right, err := stat(b)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(left, right), nil
}

func pathsOverlap(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == b || strings.HasPrefix(a, b+string(os.PathSeparator)) || strings.HasPrefix(b, a+string(os.PathSeparator))
}

func validateCleanupPath(root *os.Root, path string) error {
	return payload.Validate(root, path)
}

// openPayloadLocked is also used on every ZIP entry open after preflight. The
// lease protects against deletion; command handles serialize ownership checks with Add.
func (uc *TorrentUseCase) openPayloadLocked(hash string, index int, path string, size int64) (*os.File, error) {
	if err := uc.checkStorageAvailable(hash); err != nil {
		return nil, err
	}
	ready, _, err := uc.engine.CheckFileDirectReady(hash, index)
	if err != nil || !ready {
		return nil, ErrDownloadNotReady
	}
	if _, pending, err := uc.repo.CleanupPaths(hash); err != nil {
		return nil, err
	} else if pending {
		return nil, ErrStorageConflict
	}
	root, err := os.OpenRoot(uc.engine.DownloadDir())
	if err != nil {
		// A missing/unmounted storage root is a server failure, not a missing file.
		return nil, fmt.Errorf("payload root unavailable: %v", err)
	}
	defer root.Close()
	if err := uc.validateOwnership(root, hash, []string{path, path + ".part"}, nil); err != nil {
		return nil, err
	}
	if err := payload.Validate(root, path); err != nil {
		return nil, fmt.Errorf("%w: invalid payload", ErrStorageConflict)
	}
	f, err := payload.Open(root, path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || st.Size() != size {
		_ = f.Close()
		return nil, ErrDownloadNotReady
	}
	return f, nil
}
