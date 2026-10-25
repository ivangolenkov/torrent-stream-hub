package usecase

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/anacrolix/torrent/storage"
	"io"
	"os"
	"path/filepath"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"
)

func (uc *TorrentUseCase) copyLocation(ctx context.Context, root *os.Root, m *repository.StorageMapping, v *repository.RecoveryRecord, j *storageLocationJournal) error {
	if err := validateStorageTree(ctx, root, j.Source.Root, j.SourceTree, nil); err != nil {
		return err
	}
	spacePath := filepath.Join(uc.engine.DownloadDir(), filepath.Dir(j.Target.Root))
	if _, err := os.Stat(spacePath); errors.Is(err, os.ErrNotExist) {
		spacePath = uc.engine.DownloadDir()
	}
	remaining := j.ExtraBytes
	if j.StagingIdentity != "" {
		for _, e := range j.Entries {
			if e.CopyIdentity == "" {
				continue
			}
			actual, err := fingerprintRecovery(ctx, root, filepath.Join(j.Staging, e.Target), nil)
			if err == nil && actual.Identity == e.CopyIdentity && actual.Size == e.Evidence.Size && actual.Digest == e.Evidence.Digest {
				remaining -= e.Evidence.Size
			}
		}
	}
	if err := uc.engine.CheckMigrationSpace(spacePath, uint64(remaining)); err != nil {
		return err
	}
	if err := ensureLocationStaging(root, v, j); err != nil {
		return err
	}
	if err := uc.locationTransition(m, v, j, v.Stage); err != nil {
		return err
	}
	for _, dir := range j.Directories {
		if err := makeLocationDirectories(root, filepath.Join(j.Staging, dir)); err != nil {
			return err
		}
	}
	// Persist the identities of newly-created empty copies BEFORE their first
	// payload write. A retry can truncate only its own interrupted copy.
	for start := 0; start < len(j.Entries); start += 64 {
		if err := uc.checkLocationVolumes(j); err != nil {
			return err
		}
		end := min(start+64, len(j.Entries))
		changed := false
		for i := start; i < end; i++ {
			e := &j.Entries[i]
			if e.CopyIdentity != "" {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			path := filepath.Join(j.Staging, e.Target)
			if err := makeLocationDirectories(root, filepath.Dir(path)); err != nil {
				return err
			}
			f, err := storage.OpenRootFile(root, path, os.O_CREATE|os.O_EXCL|os.O_RDWR)
			if errors.Is(err, os.ErrExist) {
				f, err = storage.OpenRootFile(root, path, os.O_RDWR)
				if err == nil {
					st, e := f.Stat()
					if e != nil || st.Size() != 0 {
						f.Close()
						return ErrRecoveryCandidateChanged
					}
				}
			}
			if err != nil {
				return err
			}
			e.CopyIdentity, err = storage.FileIdentity(f)
			closeErr := f.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			changed = true
		}
		if changed {
			if err := uc.locationTransition(m, v, j, v.Stage); err != nil {
				return err
			}
		}
		for i := start; i < end; i++ {
			e := &j.Entries[i]
			uc.storageFile(v.Hash, e.Source, int64ptr(e.Evidence.Size))
			if err := copyLocationFile(ctx, root, j.Source.Root, j.Staging, e, func(n int64) { uc.storageBytes(v.Hash, n) }); err != nil {
				return err
			}
			uc.storageFileDone(v.Hash)
			if err := uc.recoveryBoundary("location_file_copied"); err != nil {
				return err
			}
		}
	}
	marker := j.Staging + ".owner"
	if err := root.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := validateCopiedLocation(ctx, root, j.Staging, j); err != nil {
		return err
	}
	return storage.SyncRootDirectory(root, j.Staging)
}

func ensureLocationStaging(root *os.Root, v *repository.RecoveryRecord, j *storageLocationJournal) error {
	if j.StagingIdentity != "" {
		id, err := directoryIdentity(root, j.Staging)
		if err != nil {
			return err
		}
		if id != j.StagingIdentity {
			return ErrRecoveryCandidateChanged
		}
		return nil
	}
	if err := makeLocationDirectories(root, j.Staging); err != nil {
		return err
	}
	dir, err := storage.OpenRootDirectory(root, j.Staging)
	if err != nil {
		return err
	}
	entries, readErr := dir.ReadDir(1)
	dir.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return readErr
	}
	if len(entries) != 0 {
		return ErrRecoveryCandidateChanged
	}
	marker := j.Staging + ".owner"
	token := fmt.Sprintf("%s:%s:%d\n", j.Source.StorageID, v.RecordID, v.Version)
	f, err := storage.OpenRootFile(root, marker, os.O_CREATE|os.O_EXCL|os.O_RDWR)
	if errors.Is(err, os.ErrExist) {
		f, err = storage.OpenRootFile(root, marker, os.O_RDWR)
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(f, 1024))
		if readErr != nil || !bytes.HasPrefix([]byte(token), data) {
			f.Close()
			return ErrRecoveryCandidateChanged
		}
		dir, openErr := storage.OpenRootDirectory(root, j.Staging)
		if openErr != nil {
			f.Close()
			return openErr
		}
		entries, readErr := dir.ReadDir(2)
		dir.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			f.Close()
			return readErr
		}
		if len(entries) != 0 {
			f.Close()
			return ErrRecoveryCandidateChanged
		}
		if _, err := f.Seek(0, 0); err != nil {
			f.Close()
			return err
		}
	}
	if err != nil {
		return err
	}
	_, err = f.WriteString(token)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := storage.SyncRootDirectory(root, j.Staging); err != nil {
		return err
	}
	j.StagingIdentity, err = directoryIdentity(root, j.Staging)
	return err
}

func copyLocationFile(ctx context.Context, root *os.Root, source, staging string, e *storageMoveEntry, observe func(int64)) (retErr error) {
	from, to := filepath.Join(source, e.Source), filepath.Join(staging, e.Target)
	actual, err := fingerprintRecovery(ctx, root, to, nil)
	if err == nil && actual.Identity == e.CopyIdentity && actual.Size == e.Evidence.Size && actual.Digest == e.Evidence.Digest {
		return storage.SyncRootPublication(root, to)
	}
	if err != nil {
		return err
	}
	if actual.Identity != e.CopyIdentity {
		return ErrRecoveryCandidateChanged
	}
	src, err := payload.Open(root, from)
	if err != nil {
		return err
	}
	defer src.Close()
	before, err := src.Stat()
	if err != nil {
		return err
	}
	id, err := storage.FileIdentity(src)
	if err != nil {
		return err
	}
	if id != e.Evidence.Identity || before.Size() != e.Evidence.Size || before.ModTime().UnixNano() != e.Evidence.Modified {
		return ErrRecoveryCandidateChanged
	}
	dst, err := storage.OpenRootFile(root, to, os.O_RDWR)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, dst.Close()) }()
	id, err = storage.FileIdentity(dst)
	if err != nil {
		return err
	}
	if id != e.CopyIdentity {
		return ErrRecoveryCandidateChanged
	}
	if actual.Size > e.Evidence.Size {
		return ErrRecoveryCandidateChanged
	}
	buffer, other := make([]byte, 128<<10), make([]byte, 128<<10)
	for offset := int64(0); offset < actual.Size; {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := int(min(int64(len(buffer)), actual.Size-offset))
		if _, err := src.ReadAt(buffer[:n], offset); err != nil {
			return err
		}
		if _, err := dst.ReadAt(other[:n], offset); err != nil {
			return err
		}
		if !bytes.Equal(buffer[:n], other[:n]) {
			return ErrRecoveryCandidateChanged
		}
		offset += int64(n)
	}
	if err := dst.Truncate(0); err != nil {
		return err
	}
	h := sha256.New()
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := src.Read(buffer)
		if n > 0 {
			h.Write(buffer[:n])
			observe(int64(n))
			total += int64(n)
			zero := true
			for _, b := range buffer[:n] {
				if b != 0 {
					zero = false
					break
				}
			}
			if zero {
				_, err = dst.Seek(int64(n), io.SeekCurrent)
			} else {
				_, err = dst.Write(buffer[:n])
			}
			if err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	after, err := src.Stat()
	if err != nil {
		return err
	}
	if total != e.Evidence.Size || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || hex.EncodeToString(h.Sum(nil)) != e.Evidence.Digest {
		return ErrRecoveryCandidateChanged
	}
	if err := dst.Truncate(total); err != nil {
		return err
	}
	if err := dst.Sync(); err != nil {
		return err
	}
	copied, err := fingerprintRecovery(ctx, root, to, observe)
	if err != nil {
		return err
	}
	if copied.Identity != e.CopyIdentity || copied.Size != e.Evidence.Size || copied.Digest != e.Evidence.Digest {
		return ErrRecoveryCandidateChanged
	}
	return storage.SyncRootDirectory(root, filepath.Dir(to))
}

func validateCopiedLocation(ctx context.Context, root *os.Root, path string, j *storageLocationJournal) error {
	actual, err := snapshotStorageTree(ctx, root, path, nil)
	if err != nil {
		return err
	}
	if actual.Identity != j.StagingIdentity {
		return ErrRecoveryCandidateChanged
	}
	expected := make(map[string]storageMoveEntry, len(j.Entries))
	for _, e := range j.Entries {
		expected[e.Target] = e
	}
	for _, e := range actual.Entries {
		if e.Directory {
			continue
		}
		want, ok := expected[e.Path]
		if !ok || want.CopyIdentity != e.Evidence.Identity || want.Evidence.Size != e.Evidence.Size || want.Evidence.Digest != e.Evidence.Digest {
			return ErrRecoveryCandidateChanged
		}
		delete(expected, e.Path)
	}
	if len(expected) != 0 {
		return ErrRecoveryCandidateChanged
	}
	return nil
}
