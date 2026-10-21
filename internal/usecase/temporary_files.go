package usecase

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"torrent-stream-hub/internal/models"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

func storagePaths(info *metainfo.Info) ([]string, error) {
	var paths []string
	for _, file := range info.UpvertedFiles() {
		path := storage.DefaultFilePathMaker(storage.FilePathMakerOpts{Info: info, File: &file})
		if err := validateRelativeDownloadPath(path); err != nil {
			return nil, err
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
	paths, _, err := uc.metadataStoragePaths(t)
	return paths, err
}

func (uc *TorrentUseCase) metadataStoragePaths(t *models.Torrent) ([]string, bool, error) {
	if raw := uc.engine.GetRawTorrent(t.Hash); raw != nil && raw.Info() != nil {
		paths, err := storagePaths(raw.Info())
		return paths, true, err
	}
	mi, err := metainfo.LoadFromFile(filepath.Join(uc.engine.ConfigDir(), "metainfo", strings.ToLower(t.Hash)+".torrent"))
	if err == nil {
		if mi.HashInfoBytes().HexString() != t.Hash {
			return nil, false, fmt.Errorf("metainfo hash mismatch")
		}
		info, err := mi.UnmarshalInfo()
		if err != nil {
			return nil, false, err
		}
		paths, err := storagePaths(&info)
		return paths, true, err
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	if len(t.Files) != 0 {
		return nil, false, fmt.Errorf("cannot prove storage layout without metainfo")
	}
	return nil, false, nil // No metadata and therefore no known data files.
}

func (uc *TorrentUseCase) deleteTemporaryLocked(hash string) error {
	t, err := uc.repo.GetTorrent(hash)
	if err != nil {
		return err
	}
	if t == nil || !t.IsTemporary {
		return fmt.Errorf("cleanup candidate is no longer temporary")
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
		raw := uc.engine.GetRawTorrent(hash)
		var known bool
		paths, known, err = uc.metadataStoragePaths(t)
		if err != nil {
			return err
		}
		if pending && !known {
			return fmt.Errorf("incomplete cleanup manifest requires metadata before retry")
		}
		if err := uc.repo.BeginCleanup(hash, paths); err != nil {
			return err
		}
		if err := uc.engine.Delete(hash); err != nil {
			return err
		}
		if raw != nil && raw.Info() != nil {
			paths, err = storagePaths(raw.Info())
			if err != nil {
				return err
			}
		}
		// Persist before the first unlink, so partial deletion is recoverable.
		if err := uc.repo.SaveCleanupPaths(hash, paths); err != nil {
			return err
		}
	} else {
		// A failed cleanup can have been explicitly resumed by the user.
		if err := uc.engine.Delete(hash); err != nil {
			return err
		}
	}
	all, err := uc.repo.GetAllTorrents()
	if err != nil {
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
	for _, path := range owned {
		absolute, err := filepath.Abs(filepath.Join(uc.engine.DownloadDir(), path))
		if err != nil {
			return err
		}
		for _, protected := range uc.engine.CleanupProtectedPaths() {
			reserved, err := filepath.Abs(protected)
			if err != nil {
				return err
			}
			if pathsOverlap(absolute, reserved) {
				return fmt.Errorf("torrent path overlaps application storage")
			}
			// SameFile also detects case/unicode aliases on macOS/Windows,
			// including a payload nested in an aliased application directory.
			if reservedInfo, err := os.Stat(reserved); err == nil {
				for part := path; part != "."; part = filepath.Dir(part) {
					if info, err := root.Stat(part); err == nil && os.SameFile(info, reservedInfo) {
						return fmt.Errorf("torrent path aliases application storage")
					}
				}
			}
		}
	}
	for _, other := range all {
		if other.Hash == hash {
			continue
		}
		others, err := uc.knownStoragePaths(other)
		if err != nil {
			return fmt.Errorf("cannot verify other torrent %s: %w", other.Hash, err)
		}
		if len(owned) > 0 && len(others) == 0 {
			return fmt.Errorf("cannot prove non-overlap with metadata-pending torrent %s", other.Hash)
		}
		for _, p := range others {
			for _, q := range []string{p, p + ".part"} {
				for _, own := range owned {
					if pathsOverlap(own, q) {
						return fmt.Errorf("shared storage path with torrent %s", other.Hash)
					}
					alias, err := storagePathsAlias(root, own, q)
					if err != nil {
						return err
					}
					if alias {
						return fmt.Errorf("aliased storage path with torrent %s", other.Hash)
					}
				}
			}
		}
	}
	// Validate the entire manifest before removing any entry.
	for _, path := range owned {
		if err := validateCleanupPath(root, path); err != nil {
			return err
		}
	}
	for _, path := range owned {
		if err := root.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for dir := filepath.Dir(path); dir != "."; dir = filepath.Dir(dir) {
			if err := root.Remove(dir); err != nil {
				break
			} // Empty directories only.
		}
	}
	return uc.repo.DeleteTorrent(hash)
}

func storagePathsAlias(root *os.Root, a, b string) (bool, error) {
	left, err := root.Stat(a)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	right, err := root.Stat(b)
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
	if err := validateRelativeDownloadPath(path); err != nil {
		return err
	}
	parts := strings.Split(filepath.Clean(path), string(os.PathSeparator))
	for i := range parts {
		stat, err := root.Lstat(filepath.Join(parts[:i+1]...))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if stat.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing cleanup through symlink")
		}
		if i == len(parts)-1 && !stat.Mode().IsRegular() {
			return fmt.Errorf("refusing cleanup of non-regular file")
		}
	}
	return nil
}
