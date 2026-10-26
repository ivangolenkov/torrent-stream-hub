package payload

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/anacrolix/torrent/storage"
)

const InstanceLockName = ".tsh-instance.lock"

var (
	ErrInstanceLocked          = errors.New("download root is already owned by another instance")
	ErrInstanceLockUnsupported = errors.New("download root instance lock is unsupported")
)

type InstanceLock struct {
	file *os.File
	once sync.Once
	err  error
}

// LockInstance locks a persistent rooted file. Never unlink it: another opener
// must not lock a new inode while an old process still owns the previous one.
// An OS lock protects cooperating instances, not arbitrary external writers.
func LockInstance(dir string) (*InstanceLock, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := storage.OpenRootFile(root, InstanceLockName, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, err
	}
	if err := lockInstanceFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock download root: %w", err)
	}
	opened, err := f.Stat()
	current, statErr := root.Lstat(InstanceLockName)
	if err != nil || statErr != nil || !os.SameFile(opened, current) {
		f.Close()
		return nil, storage.ErrUnsafeFile
	}
	return &InstanceLock{file: f}, nil
}

func (l *InstanceLock) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() { l.err = l.file.Close() })
	return l.err
}
