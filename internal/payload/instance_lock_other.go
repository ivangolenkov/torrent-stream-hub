//go:build !linux && !darwin && !windows

package payload

import "os"

func lockInstanceFile(*os.File) error { return ErrInstanceLockUnsupported }
