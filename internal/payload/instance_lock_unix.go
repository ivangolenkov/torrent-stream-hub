//go:build linux || darwin

package payload

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockInstanceFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return ErrInstanceLocked
	}
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.ENOTSUP) {
		return ErrInstanceLockUnsupported
	}
	return err
}
