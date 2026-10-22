// Package payload defines the current library storage layout and root-contained
// filesystem access shared by streaming, downloads and cleanup.
package payload

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

var ErrNotRegular = errors.New("payload is not a regular file")

func Path(info *metainfo.Info, file *metainfo.FileInfo) string {
	return storage.DefaultFilePathMaker(storage.FilePathMakerOpts{Info: info, File: file})
}

const LayoutVersion = 1

func Root(hash string) string { return filepath.Join("payload", hash) }

func ValidRoot(root string) bool {
	parts := strings.Split(filepath.Clean(root), string(os.PathSeparator))
	if len(parts) != 2 || parts[0] != "payload" || len(parts[1]) != 40 {
		return false
	}
	_, err := hex.DecodeString(parts[1])
	return err == nil
}

func Overlap(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	return a == b || strings.HasPrefix(a, b+string(os.PathSeparator)) || strings.HasPrefix(b, a+string(os.PathSeparator))
}

func IsolatedPath(hash string, info *metainfo.Info, file *metainfo.FileInfo) string {
	return filepath.Join(Root(hash), Path(info, file))
}

func ValidateRelative(name string) error {
	if !filepath.IsLocal(name) || filepath.Clean(name) == "." {
		return fmt.Errorf("unsafe relative payload path")
	}
	return nil
}

// Validate checks every component without following symlinks. Missing entries
// are allowed for idempotent cleanup; Open still requires the final file.
func Validate(root *os.Root, name string) error {
	if err := ValidateRelative(name); err != nil {
		return err
	}
	parts := strings.Split(filepath.Clean(name), string(os.PathSeparator))
	for i := range parts {
		st, err := root.Lstat(filepath.Join(parts[:i+1]...))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("payload symlink rejected")
		}
		if i == len(parts)-1 && !st.Mode().IsRegular() {
			return ErrNotRegular
		}
	}
	return nil
}

// Open never falls back to an absolute path. Root containment remains enforced
// by the actual open even if a parent is replaced after validation.
func Open(root *os.Root, name string) (*os.File, error) {
	if err := Validate(root, name); err != nil {
		return nil, err
	}
	expected, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !expected.Mode().IsRegular() {
		return nil, ErrNotRegular
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || storage.FileHasMultipleLinks(f, st) {
		_ = f.Close()
		return nil, ErrNotRegular
	}
	if !os.SameFile(expected, st) {
		_ = f.Close()
		return nil, fmt.Errorf("payload changed during open")
	}
	return f, nil
}
