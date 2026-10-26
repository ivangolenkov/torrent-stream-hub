package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// LegacyPartConflictError is non-destructive. Path is relative to the torrent
// metadata root; callers must not infer a preferred candidate from this error.
type LegacyPartConflictError struct{ Path string }

func (e *LegacyPartConflictError) Error() string {
	return "legacy partial file conflicts with existing payload; both files preserved"
}

// Import is idempotent: an atomic no-replace rename either leaves the old name
// or the metadata name. Verification never trusts either name or persisted hints.
// Preflight the entire catalog before renaming, so a known conflict leaves every
// file untouched. Interrupted imports can resume on the next open.
func (fs *fileClientImpl) importPartFiles(ctx context.Context, files []fileExtra) error {
	return fs.importPartFilesWithBarrier(ctx, files, nil)
}

func (fs *fileClientImpl) importPartFilesWithBarrier(ctx context.Context, files []fileExtra, resume *FileResumeHooks) error {
	normalize := func(path string) string { return strings.ToLower(norm.NFC.String(path)) }
	payloadNames := make(map[string]bool, len(files))
	payloadParents := make(map[string]bool)
	for i := range files {
		f := &files[i]
		name := normalize(f.safeOsPath)
		payloadNames[name] = true
		for parent := filepath.Dir(name); ; parent = filepath.Dir(parent) {
			payloadParents[parent] = true
			if filepath.Dir(parent) == parent || parent == "." {
				break
			}
		}
	}
	r := rootFileIo{root: fs.opts.RootFS, ctx: fs.opts.IOContext}
	var pending []int
	for i := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		f := &files[i]
		candidate := normalize(f.partFilePath())
		// A real torrent file/directory can itself end in .part. Metadata owns
		// those names; never reinterpret them as an older file's partial data.
		owned := payloadNames[candidate] || payloadParents[candidate]
		for parent := filepath.Dir(candidate); !owned; parent = filepath.Dir(parent) {
			owned = payloadNames[parent]
			if filepath.Dir(parent) == parent || parent == "." {
				break
			}
		}
		if owned {
			continue
		}
		if _, err := r.stat(f.partFilePath()); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("checking legacy partial file %q: %w", f.partFilePath(), err)
		}
		if _, err := r.stat(f.safeOsPath); err == nil {
			return &LegacyPartConflictError{Path: f.safeOsPath}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		pending = append(pending, i)
	}
	if len(pending) != 0 && resume != nil {
		if err := resume.Invalidate(); err != nil {
			return err
		}
		clear(resume.Pending)
		resume.RestoreCandidate = false
		resume.Observation.Status, resume.Observation.Reason = "fallback", "identity_changed"
	}
	for _, i := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		f := &files[i]
		if err := r.flush(f.partFilePath(), 0, 0); err != nil {
			return fmt.Errorf("flushing legacy partial file: %w", err)
		}
		if err := r.rename(f.partFilePath(), f.safeOsPath); err != nil {
			return fmt.Errorf("importing legacy partial file %q: %w", f.partFilePath(), err)
		}
	}
	return nil
}
