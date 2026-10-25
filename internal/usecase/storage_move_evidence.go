package usecase

import (
	"context"
	"errors"
	"fmt"
	"github.com/anacrolix/torrent/storage"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"
)

const maxStorageManifestEntries = 65536

type storageTreeEntry struct {
	Path      string              `json:"path"`
	Directory bool                `json:"directory,omitempty"`
	Evidence  recoveryFingerprint `json:"evidence"`
}
type storageTree struct {
	Identity string             `json:"identity"`
	Entries  []storageTreeEntry `json:"entries"`
}
type storageMoveEntry struct {
	Source       string              `json:"source"`
	Target       string              `json:"target"`
	Evidence     recoveryFingerprint `json:"evidence"`
	CopyIdentity string              `json:"copy_identity,omitempty"`
	Removed      bool                `json:"removed,omitempty"`
}
type storageLocationJournal struct {
	FreeBytes       *uint64                    `json:"free_bytes,omitempty"`
	CandidateTree   storageTree                `json:"candidate_tree,omitempty"`
	Analysis        *models.LocationAnalysis   `json:"analysis,omitempty"`
	CleanupPath     string                     `json:"cleanup_path,omitempty"`
	CleanupIdentity string                     `json:"cleanup_identity,omitempty"`
	CopiesComplete  bool                       `json:"copies_complete,omitempty"`
	Directories     []string                   `json:"directories,omitempty"`
	Schema          int                        `json:"schema"`
	Source          *repository.StorageMapping `json:"source_mapping"`
	Target          payload.Layout             `json:"target"`
	Staging         string                     `json:"staging"`
	StagingIdentity string                     `json:"staging_identity,omitempty"`
	SourceTree      storageTree                `json:"source_tree"`
	Entries         []storageMoveEntry         `json:"entries"`
	Method          string                     `json:"method"`
	ExtraBytes      int64                      `json:"extra_bytes"`
	RetainSource    bool                       `json:"retain_source"`
	Alternatives    []preservedAlternative     `json:"alternatives,omitempty"`
}

// One accounted worker owns this scan. Reads use batches of 128 directory
// entries, a 128 KiB hash buffer, and a hard manifest/depth/path-byte budget.
// The snapshot is evidence for a storage operation, never piece completion.
func snapshotStorageTree(ctx context.Context, root *os.Root, path string, observe func(int64)) (storageTree, error) {
	var tree storageTree
	dir, err := storage.OpenRootDirectory(root, path)
	if errors.Is(err, os.ErrNotExist) {
		return tree, nil
	}
	if err != nil {
		return tree, err
	}
	before, err := dir.Stat()
	if err != nil {
		dir.Close()
		return tree, err
	}
	tree.Identity, err = storage.FileIdentity(dir)
	dir.Close()
	if err != nil {
		return tree, err
	}
	pathBytes := 0
	var walk func(string, int) error
	walk = func(relative string, depth int) error {
		if depth > 128 {
			return fmt.Errorf("storage manifest depth limit")
		}
		f, err := storage.OpenRootDirectory(root, filepath.Join(path, relative))
		if err != nil {
			return err
		}
		defer f.Close()
		for {
			batch, readErr := f.ReadDir(128)
			for _, entry := range batch {
				if err := ctx.Err(); err != nil {
					return err
				}
				rel := filepath.Join(relative, entry.Name())
				pathBytes += len(rel)
				if len(tree.Entries) >= maxStorageManifestEntries || pathBytes > 16<<20 {
					return fmt.Errorf("storage manifest size limit")
				}
				full := filepath.Join(path, rel)
				item := storageTreeEntry{Path: rel, Directory: entry.IsDir()}
				if entry.Type()&os.ModeSymlink != 0 {
					return payload.ErrUnsafePath
				}
				if item.Directory {
					d, err := storage.OpenRootDirectory(root, full)
					if err != nil {
						return err
					}
					item.Evidence.Identity, err = storage.FileIdentity(d)
					d.Close()
					if err != nil {
						return err
					}
				} else {
					item.Evidence, err = fingerprintRecovery(ctx, root, full, observe)
					if err != nil {
						return err
					}
				}
				tree.Entries = append(tree.Entries, item)
				if item.Directory {
					if err := walk(rel, depth+1); err != nil {
						return err
					}
				}
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				return readErr
			}
		}
		return nil
	}
	if err := walk(".", 0); err != nil {
		return tree, err
	}
	current, err := storage.OpenRootDirectory(root, path)
	if err != nil {
		return tree, err
	}
	defer current.Close()
	after, err := current.Stat()
	if err != nil {
		return tree, err
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		return tree, ErrRecoveryCandidateChanged
	}
	sort.Slice(tree.Entries, func(i, j int) bool { return tree.Entries[i].Path < tree.Entries[j].Path })
	return tree, nil
}

func validateStorageTree(ctx context.Context, root *os.Root, path string, expected storageTree, observe func(int64)) error {
	actual, err := snapshotStorageTree(ctx, root, path, observe)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, expected) {
		return ErrRecoveryCandidateChanged
	}
	return nil
}

func directoryIdentity(root *os.Root, path string) (string, error) {
	f, err := storage.OpenRootDirectory(root, path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return storage.FileIdentity(f)
}
