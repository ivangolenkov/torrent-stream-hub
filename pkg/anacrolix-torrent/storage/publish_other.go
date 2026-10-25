//go:build !darwin && !linux && !windows

package storage

import "os"

func renameNoReplace(*os.File, string, string) error                  { return os.ErrInvalid }
func renameBetweenNoReplace(*os.File, *os.File, string, string) error { return os.ErrInvalid }
func syncPublicationDirectory(*os.File) error                         { return nil }
