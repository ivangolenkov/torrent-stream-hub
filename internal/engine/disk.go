package engine

import (
	"golang.org/x/sys/unix"
)

// GetFreeSpace returns the free space in bytes for a given path
func GetFreeSpace(path string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	// Available blocks * size per block = available space in bytes
	return stat.Bavail * uint64(stat.Bsize), nil
}
