package storage

import (
	"golang.org/x/sys/unix"
	"os"
)

func renameNoReplace(dir *os.File, from, to string) error {
	return unix.Renameat2(int(dir.Fd()), from, int(dir.Fd()), to, unix.RENAME_NOREPLACE)
}
func syncPublicationDirectory(dir *os.File) error { return dir.Sync() }
