//go:build !windows

package storage

import (
	"os"
	"syscall"
)

func FileHasMultipleLinks(_ *os.File, st os.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && s.Nlink > 1
}
