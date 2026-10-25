package storage

import (
	"golang.org/x/sys/unix"
	"os"
)

func renameNoReplace(dir *os.File, from, to string) error {
	return renameBetweenNoReplace(dir, dir, from, to)
}
func renameBetweenNoReplace(fromDir, toDir *os.File, from, to string) error {
	return unix.RenameatxNp(int(fromDir.Fd()), from, int(toDir.Fd()), to, unix.RENAME_EXCL)
}
func syncPublicationDirectory(dir *os.File) error { return dir.Sync() }
