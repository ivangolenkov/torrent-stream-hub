package storage

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

var ErrPublicationFlush = errors.New("storage publication flush failed")

func publicationFlushError(err error) error {
	if err == nil {
		return nil
	}
	return errors.Join(ErrPublicationFlush, err)
}

// MoveRootNoReplace moves a single regular, single-link file between rooted
// directories on the same filesystem. No overwrite/copy/delete fallback exists.
// A sync error can follow a successful rename: journal callers must inspect both
// names on retry, never assume an error means the namespace was unchanged.
func MoveRootNoReplace(root *os.Root, from, to string) error {
	r := rootFileIo{root: root}
	flags := os.O_RDONLY
	if runtime.GOOS == "windows" {
		flags = os.O_RDWR
	}
	source, err := OpenRootFile(root, from, flags)
	if err != nil {
		return err
	}
	err = source.Sync()
	err = publicationFlushError(errors.Join(err, source.Close()))
	if err != nil {
		return err
	}
	for _, p := range []string{from, to} {
		if _, err := r.relative(filepath.Join(root.Name(), p)); err != nil {
			return err
		}
	}
	if _, err := root.Lstat(to); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	left, err := root.Open(filepath.Dir(from))
	if err != nil {
		return err
	}
	defer left.Close()
	right, err := root.Open(filepath.Dir(to))
	if err != nil {
		return err
	}
	defer right.Close()
	if err := renameBetweenNoReplace(left, right, filepath.Base(from), filepath.Base(to)); err != nil {
		return err
	}
	return publicationFlushError(errors.Join(syncPublicationDirectory(left), syncPublicationDirectory(right)))
}

func SyncRootPublication(root *os.Root, path string) error {
	flags := os.O_RDONLY
	if runtime.GOOS == "windows" {
		flags = os.O_RDWR
	}
	f, err := OpenRootFile(root, path, flags)
	if err != nil {
		return err
	}
	err = publicationFlushError(errors.Join(f.Sync(), f.Close()))
	if err != nil {
		return err
	}
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		dir, err := root.Open(parent)
		if err != nil {
			return err
		}
		err = publicationFlushError(errors.Join(syncPublicationDirectory(dir), dir.Close()))
		if err != nil {
			return err
		}
		if parent == "." {
			break
		}
	}
	return nil
}

func SyncRootDirectory(root *os.Root, path string) error {
	dir, err := root.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(syncPublicationDirectory(dir), dir.Close())
}
