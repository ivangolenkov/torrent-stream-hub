//go:build !linux && !darwin && !windows

package storage

import "os"

func FileIdentity(*os.File) (string, error) { return "", os.ErrInvalid }
