package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/anacrolix/torrent/storage"
	"io"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"
)

func (uc *TorrentUseCase) executeLocation(ctx context.Context, root *os.Root, m *repository.StorageMapping, v *repository.RecoveryRecord, j *storageLocationJournal) error {
	claim, err := uc.reserveOwnership(ctx, root, v.Hash, []string{j.Source.Root, j.Target.Root})
	if err != nil {
		return err
	}
	defer claim()
	for {
		if err := uc.checkLocationVolumes(j); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		switch v.Stage {
		case "planning":
			return uc.planLocation(ctx, root, m, v, j)
		case "planned", "completed", "cancelled", "cancel_requested":
			return nil
		case "queued":
			if err := uc.locationTransition(m, v, j, "waiting_readers"); err != nil {
				return err
			}
		case "waiting_readers":
			if err := uc.waitLocationReaders(ctx, v.Hash); err != nil {
				return err
			}
			if err := uc.engine.Delete(v.Hash); err != nil {
				return err
			}
			if err := uc.locationTransition(m, v, j, "prepared"); err != nil {
				return err
			}
		case "prepared":
			if err := validateStorageTree(ctx, root, j.Source.Root, j.SourceTree, func(n int64) { uc.storageBytes(v.Hash, n) }); err != nil {
				return err
			}
			occupied, err := namedPathOccupied(root, j.Target.Root)
			if err != nil {
				return err
			}
			if occupied {
				return os.ErrExist
			}
			if j.Method == "rename" {
				for _, e := range j.Entries {
					if err := storage.SyncRootPublication(root, filepath.Join(j.Source.Root, e.Source)); err != nil {
						return err
					}
				}
				for _, entry := range j.SourceTree.Entries {
					if entry.Directory {
						if err := storage.SyncRootDirectory(root, filepath.Join(j.Source.Root, entry.Path)); err != nil {
							return err
						}
					}
				}
				if err := storage.SyncRootDirectory(root, j.Source.Root); err != nil {
					return err
				}
				if err := uc.locationTransition(m, v, j, "publishing"); err != nil {
					return err
				}
			} else {
				if err := uc.locationTransition(m, v, j, "copying"); err != nil {
					return err
				}
			}
		case "copying":
			if err := uc.copyLocation(ctx, root, m, v, j); err != nil {
				return err
			}
			j.CopiesComplete = true
			if err := uc.locationTransition(m, v, j, "publishing"); err != nil {
				return err
			}
		case "publishing":
			if err := uc.publishLocation(ctx, root, m, v, j); err != nil {
				return err
			}
			if err := uc.locationTransition(m, v, j, "published"); err != nil {
				return err
			}
		case "published":
			if err := uc.validateLocationTarget(ctx, root, j); err != nil {
				return err
			}
			if err := uc.repo.CommitStorageOperation(ctx, j.Source, v, j.Target, false); err != nil {
				return err
			}
			v.Stage = "committed"
			if err := uc.recoveryBoundary("location_mapping_committed"); err != nil {
				return err
			}
			m, err = uc.repo.StorageMapping(v.Hash)
			if err != nil {
				return err
			}
		case "committed":
			if err := uc.locationTransition(m, v, j, "cleanup"); err != nil {
				return err
			}
		case "cleanup":
			if err := uc.cleanupLocation(ctx, root, m, v, j); err != nil {
				return err
			}
			retained := []string{}
			if _, err := root.Lstat(j.Source.Root); err == nil {
				retained = append(retained, j.Source.Root)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			v.Stage = "completed"
			data, err := json.Marshal(j)
			if err != nil {
				return err
			}
			v.Journal = string(data)
			if err := uc.repo.FinishStorageOperation(m, v, "cleanup", retained); err != nil {
				return err
			}
			if err := uc.recoveryBoundary("location_completed"); err != nil {
				return err
			}
			return uc.restoreAfterLocation(ctx, v)
		default:
			return fmt.Errorf("unsupported storage journal stage %q", v.Stage)
		}
	}
}

func (uc *TorrentUseCase) planLocation(ctx context.Context, root *os.Root, m *repository.StorageMapping, v *repository.RecoveryRecord, j *storageLocationJournal) error {
	occupied, err := namedPathOccupied(root, j.Target.Root)
	if err != nil {
		return err
	}
	if occupied {
		return os.ErrExist
	}
	tree, err := snapshotStorageTree(ctx, root, m.Root, func(n int64) { uc.storageBytes(v.Hash, n) })
	if err != nil {
		return err
	}
	j.SourceTree = tree
	parent := filepath.Dir(j.Target.Root)
	for {
		if _, err := root.Lstat(parent); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if parent == "." {
			return os.ErrNotExist
		}
		parent = filepath.Dir(parent)
	}
	free, err := engine.GetFreeSpace(filepath.Join(root.Name(), parent))
	if err != nil {
		return err
	}
	j.FreeBytes = &free
	j.Entries = nil
	j.Directories = nil
	j.ExtraBytes = 0
	j.RetainSource = false
	j.Method = "copy"
	if m.Prefix == j.Target.Prefix && tree.Identity != "" {
		j.Method = "rename"
	}
	for _, e := range tree.Entries {
		if uc.isVolumeMarker(filepath.Join(m.Root, e.Path)) {
			j.RetainSource = true
			j.Method = "copy"
			continue
		}
		metadataPath := e.Path
		if m.Prefix != "" {
			metadataPath = filepath.Join(m.Prefix, e.Path)
		}
		target, err := j.Target.Relative(metadataPath)
		if err != nil {
			if e.Directory && metadataPath == j.Target.Prefix {
				continue
			}
			j.RetainSource = true
			continue
		}
		if e.Directory {
			j.Directories = append(j.Directories, target)
			continue
		}
		if e.Evidence.Size > math.MaxInt64-j.ExtraBytes {
			return fmt.Errorf("storage size overflow")
		}
		j.ExtraBytes += e.Evidence.Size
		j.Entries = append(j.Entries, storageMoveEntry{Source: e.Path, Target: target, Evidence: e.Evidence})
	}
	return uc.locationTransition(m, v, j, "planned")
}

func (uc *TorrentUseCase) publishLocation(ctx context.Context, root *os.Root, m *repository.StorageMapping, v *repository.RecoveryRecord, j *storageLocationJournal) error {
	from, expected := j.Source.Root, j.SourceTree.Identity
	if j.Method == "copy" {
		from, expected = j.Staging, j.StagingIdentity
	}
	targetID, targetErr := directoryIdentity(root, j.Target.Root)
	if targetErr == nil {
		if targetID != expected {
			return os.ErrExist
		}
		if _, err := root.Lstat(from); !errors.Is(err, os.ErrNotExist) {
			if err != nil {
				return err
			}
			return ErrRecoveryCandidateChanged
		}
		if err := uc.validateLocationTarget(ctx, root, j); err != nil {
			return err
		}
		return errors.Join(storage.SyncRootDirectory(root, filepath.Dir(j.Target.Root)), storage.SyncRootDirectory(root, filepath.Dir(j.Source.Root)))
	}
	if !errors.Is(targetErr, os.ErrNotExist) {
		return targetErr
	}
	if j.Method == "copy" {
		if !j.CopiesComplete {
			if err := uc.copyLocation(ctx, root, m, v, j); err != nil {
				return err
			}
			j.CopiesComplete = true
			if err := uc.locationTransition(m, v, j, "publishing"); err != nil {
				return err
			}
		} else {
			if err := validateCopiedLocation(ctx, root, j.Staging, j); err != nil {
				return err
			}
			if err := validateStorageTree(ctx, root, j.Source.Root, j.SourceTree, nil); err != nil {
				return err
			}
		}
		expected = j.StagingIdentity
	}

	if j.Method == "rename" {
		if err := validateStorageTree(ctx, root, from, j.SourceTree, nil); err != nil {
			return err
		}
	}
	if id, err := directoryIdentity(root, from); err != nil {
		return err
	} else if id != expected {
		return ErrRecoveryCandidateChanged
	}
	if err := makeLocationDirectories(root, filepath.Dir(j.Target.Root)); err != nil {
		return err
	}
	move := uc.locationMove
	if move == nil {
		move = storage.MoveRootDirectoryNoReplace
	}
	err := move(root, from, j.Target.Root)
	if j.Method == "rename" && storage.IsCrossDeviceRename(err) {
		j.Method = "copy"
		if err := uc.locationTransition(m, v, j, "publishing"); err != nil {
			return err
		}
		return uc.publishLocation(ctx, root, m, v, j)
	}
	if err != nil {
		return err
	}
	return uc.recoveryBoundary("location_namespace_published")
}

func (uc *TorrentUseCase) validateLocationTarget(ctx context.Context, root *os.Root, j *storageLocationJournal) error {
	if j.Method == "rename" {
		return validateStorageTree(ctx, root, j.Target.Root, j.SourceTree, nil)
	}
	id, err := directoryIdentity(root, j.Target.Root)
	if err != nil {
		return err
	}
	if id != j.StagingIdentity {
		return ErrRecoveryCandidateChanged
	}
	return validateCopiedLocation(ctx, root, j.Target.Root, j)
}

func (uc *TorrentUseCase) cleanupLocation(ctx context.Context, root *os.Root, m *repository.StorageMapping, v *repository.RecoveryRecord, j *storageLocationJournal) error {
	if err := uc.validateLocationTarget(ctx, root, j); err != nil {
		return err
	}
	if j.Method == "rename" {
		return nil
	}
	id, err := directoryIdentity(root, j.Source.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if id != j.SourceTree.Identity {
		return ErrRecoveryCandidateChanged
	}
	if j.CleanupPath == "" {
		j.CleanupPath = filepath.Join(j.Source.Root, fmt.Sprintf(".tsh-cleanup-%s-%d", v.RecordID, v.Version))
	}
	cleanup := &storageLocationJournal{Source: j.Source, Staging: j.CleanupPath, StagingIdentity: j.CleanupIdentity}
	allRemoved := true
	for _, entry := range j.Entries {
		if !entry.Removed {
			allRemoved = false
			break
		}
	}
	if !allRemoved {
		if err := ensureLocationStaging(root, v, cleanup); err != nil {
			return err
		}
		if j.CleanupIdentity == "" {
			j.CleanupIdentity = cleanup.StagingIdentity
			if err := uc.locationTransition(m, v, j, "cleanup"); err != nil {
				return err
			}
		}
	} else if id, err := directoryIdentity(root, j.CleanupPath); err == nil && id != j.CleanupIdentity {
		return ErrRecoveryCandidateChanged
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for index := range j.Entries {
		if err := uc.checkLocationVolumes(j); err != nil {
			return err
		}
		e := &j.Entries[index]
		if e.Removed {
			continue
		}
		path := filepath.Join(j.Source.Root, e.Source)
		quarantine := filepath.Join(j.CleanupPath, fmt.Sprintf("%d", index))
		if _, err := root.Lstat(quarantine); errors.Is(err, os.ErrNotExist) {
			if _, err := root.Lstat(path); errors.Is(err, os.ErrNotExist) {
				e.Removed = true
				continue
			} else if err != nil {
				return err
			}
			if err := matchesRecovery(ctx, root, path, e.Evidence, nil); err != nil {
				return err
			}
			if err := storage.MoveRootNoReplace(root, path, quarantine); err != nil {
				return err
			}
			if err := uc.recoveryBoundary("location_source_quarantined"); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		// Revalidate AFTER the no-replace move. A replaced public source name
		// is preserved here instead of being unlinked as if it were the old file.
		if err := matchesRecovery(ctx, root, quarantine, e.Evidence, func(n int64) { uc.storageBytes(v.Hash, n) }); err != nil {
			return err
		}
		copy, err := fingerprintRecovery(ctx, root, filepath.Join(j.Target.Root, e.Target), nil)
		if err != nil {
			return err
		}
		if copy.Identity != e.CopyIdentity || copy.Size != e.Evidence.Size || copy.Digest != e.Evidence.Digest {
			return ErrRecoveryCandidateChanged
		}
		if err := root.Remove(quarantine); err != nil {
			return err
		}
		if err := storage.SyncRootDirectory(root, j.CleanupPath); err != nil {
			return err
		}
		e.Removed = true
		if err := uc.recoveryBoundary("location_source_removed"); err != nil {
			return err
		}
		if (index+1)%64 == 0 {
			if err := uc.locationTransition(m, v, j, "cleanup"); err != nil {
				return err
			}
		}
	}
	if err := uc.locationTransition(m, v, j, "cleanup"); err != nil {
		return err
	}
	if err := root.Remove(j.CleanupPath); err != nil && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := storage.SyncRootDirectory(root, j.Source.Root); err != nil {
		return err
	}
	if err := uc.recoveryBoundary("location_cleanup_directory_removed"); err != nil {
		return err
	}
	marker := j.CleanupPath + ".owner"
	if f, err := payload.Open(root, marker); err == nil {
		data, readErr := io.ReadAll(io.LimitReader(f, 1024))
		f.Close()
		if readErr == nil && string(data) == fmt.Sprintf("%s:%s:%d\n", j.Source.StorageID, v.RecordID, v.Version) {
			if err := root.Remove(marker); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for index := len(j.SourceTree.Entries) - 1; index >= 0; index-- {
		e := j.SourceTree.Entries[index]
		if !e.Directory {
			continue
		}
		metadataPath := filepath.Join(j.Source.Prefix, e.Path)
		if _, err := j.Target.Relative(metadataPath); err != nil && metadataPath != j.Target.Prefix {
			continue // Unmapped empty directories are retained with their owner.
		}
		path := filepath.Join(j.Source.Root, e.Path)
		actual, err := directoryIdentity(root, path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if actual != e.Evidence.Identity {
			return ErrRecoveryCandidateChanged
		}
		if err := root.Remove(path); err == nil {
			if err := storage.SyncRootDirectory(root, filepath.Dir(path)); err != nil {
				return err
			}
		} else if !errors.Is(err, syscall.ENOTEMPTY) {
			return err
		}
	}
	if err := root.Remove(j.Source.Root); err != nil && !errors.Is(err, syscall.ENOTEMPTY) {
		return err
	}
	return storage.SyncRootDirectory(root, filepath.Dir(j.Source.Root))
}

func (uc *TorrentUseCase) finishLocationCancel(ctx context.Context, root *os.Root, m *repository.StorageMapping, v *repository.RecoveryRecord, j *storageLocationJournal) {
	// Cancellation preserves staged data under a durable retained reservation.
	// It never recursively deletes an interrupted or externally changed tree.
	retained := []string{}
	if root == nil {
		retained = append(retained, j.Staging)
	} else if _, err := root.Lstat(j.Staging); err == nil || !errors.Is(err, os.ErrNotExist) {
		retained = append(retained, j.Staging)
	}
	if root != nil {
		if _, err := root.Lstat(j.Staging + ".owner"); err == nil {
			retained = append(retained, j.Staging+".owner")
		}
	}
	v.Stage = "cancelled"
	if err := uc.repo.FinishStorageOperation(m, v, "cancel_requested", retained); err != nil {
		return
	}
	_ = uc.recoveryBoundary("location_cancelled")
	if !uc.engine.StorageMetadata(v.Hash).Exists() {
		_ = uc.restoreAfterLocation(ctx, v)
	}
}

func makeLocationDirectories(root *os.Root, path string) error {
	if path == "." {
		return nil
	}
	if err := payload.ValidateRelative(path); err != nil {
		return err
	}
	parent := filepath.Dir(path)
	if parent != path {
		if err := makeLocationDirectories(root, parent); err != nil {
			return err
		}
	}
	createdErr := root.Mkdir(path, 0700)
	if createdErr != nil && !errors.Is(createdErr, os.ErrExist) {
		return createdErr
	}
	f, err := storage.OpenRootDirectory(root, path)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if createdErr == nil {
		return storage.SyncRootDirectory(root, parent)
	}
	return nil
}
