//go:build linux || darwin

package storage

import (
	"fmt"
	"os"
	"syscall"
)

func FileIdentity(f *os.File) (string, error) {
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	v, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return "", os.ErrInvalid
	}
	return fmt.Sprintf("%x:%x", v.Dev, v.Ino), nil
}

// FilesystemIdentity is only a live mount comparison, not a durable volume ID.
func FilesystemIdentity(f *os.File) (string, error) {
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	v, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return "", os.ErrInvalid
	}
	return fmt.Sprintf("%x", v.Dev), nil
}
