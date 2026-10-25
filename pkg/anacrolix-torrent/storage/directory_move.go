package storage

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

func IsCrossDeviceRename(err error) bool {
	if runtime.GOOS == "windows" {
		return errors.Is(err, syscall.Errno(17))
	} // ERROR_NOT_SAME_DEVICE
	return errors.Is(err, syscall.EXDEV)
}

// OpenRootDirectory refuses symlinks at every component and verifies the opened
// identity. os.Root additionally enforces containment during actual traversal.
func OpenRootDirectory(root *os.Root, path string) (*os.File, error) {
	return openRootDirectoryObserved(root, path, nil)
}

func openRootDirectoryObserved(root *os.Root, path string, observe func(string, string)) (*os.File, error) {
	event := func(op, name string) {
		if observe != nil {
			observe(op, name)
		}
	}
	if !filepath.IsLocal(path) {
		return nil, ErrUnsafeFile
	}
	path = filepath.Clean(path)
	var before os.FileInfo
	if path != "." {
		parts := strings.Split(path, string(filepath.Separator))
		for i := range parts {
			event("lstat", filepath.Join(parts[:i+1]...))
			st, err := root.Lstat(filepath.Join(parts[:i+1]...))
			if err != nil {
				return nil, err
			}
			if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
				return nil, ErrUnsafeFile
			}
			before = st
		}
	}
	if before == nil {
		var err error
		event("stat", path)
		before, err = root.Stat(path)
		if err != nil {
			return nil, err
		}
	}
	event("open", path)
	f, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	event("fstat", path)
	after, err := f.Stat()
	if err != nil || !after.IsDir() || !os.SameFile(before, after) {
		f.Close()
		return nil, ErrUnsafeFile
	}
	return f, nil
}

// MoveRootDirectoryNoReplace is a native same-filesystem directory rename.
// The caller owns and flushes the payload tree before this namespace operation.
// A returned flush error can follow success; a journal must inspect both names.
func MoveRootDirectoryNoReplace(root *os.Root, from, to string) error {
	if !filepath.IsLocal(from) || !filepath.IsLocal(to) || filepath.Clean(from) == "." || filepath.Clean(to) == "." {
		return ErrUnsafeFile
	}
	source, err := OpenRootDirectory(root, from)
	if err != nil {
		return err
	}
	if err := source.Close(); err != nil {
		return err
	}
	if _, err := root.Lstat(to); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	left, err := OpenRootDirectory(root, filepath.Dir(from))
	if err != nil {
		return err
	}
	defer left.Close()
	right, err := OpenRootDirectory(root, filepath.Dir(to))
	if err != nil {
		return err
	}
	defer right.Close()
	if err := renameBetweenNoReplace(left, right, filepath.Base(from), filepath.Base(to)); err != nil {
		return err
	}
	return publicationFlushError(errors.Join(syncPublicationDirectory(left), syncPublicationDirectory(right)))
}
