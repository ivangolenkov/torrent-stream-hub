package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"

	"torrent-stream-hub/internal/payload"
)

type StreamSourceType string

const (
	StreamSourceDisk    StreamSourceType = "disk"
	StreamSourceTorrent StreamSourceType = "torrent"
)

type FallbackReason string

const (
	FallbackReasonNone               FallbackReason = ""
	FallbackReasonNotReady           FallbackReason = "not_ready"
	FallbackReasonPartFileExists     FallbackReason = "part_file_exists"
	FallbackReasonFinalFileNotFound  FallbackReason = "final_file_not_found"
	FallbackReasonSizeMismatch       FallbackReason = "size_mismatch"
	FallbackReasonNotRegularFile     FallbackReason = "not_regular_file"
	FallbackReasonPathOutsideRoot    FallbackReason = "path_outside_root"
	FallbackReasonOpenFailed         FallbackReason = "open_failed"
	FallbackReasonReadinessRevoked   FallbackReason = "readiness_revoked_after_open"
	FallbackReasonStorageUnsupported FallbackReason = "storage_unsupported"
)

// StreamSource encapsulates an opened stream source with ownership and cleanup lifecycle.
type StreamSource struct {
	Type           StreamSourceType
	Reader         io.ReadSeekCloser
	Length         int64
	DisplayName    string
	FallbackReason FallbackReason
	cleanupOnce    sync.Once
	cleanupFn      func() error
}

func (s *StreamSource) Close() error {
	if s == nil || s.cleanupFn == nil {
		return nil
	}
	var err error
	s.cleanupOnce.Do(func() {
		err = s.cleanupFn()
	})
	return err
}

// directDiskReader wraps an OS file to provide context-aware, cancelable reading without
// implementing optional bypass interfaces like io.WriterTo.
type directDiskReader struct {
	file       *os.File
	ctx        context.Context
	cancelChan <-chan struct{}
	closed     atomic.Bool
	onClose    func()
}

func (r *directDiskReader) Read(p []byte) (n int, err error) {
	if r.closed.Load() {
		return 0, os.ErrClosed
	}
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case <-r.cancelChan:
		return 0, errors.New("stream cancelled: torrent closed or deleted")
	default:
	}

	return r.file.Read(p)
}

func (r *directDiskReader) Seek(offset int64, whence int) (int64, error) {
	if r.closed.Load() {
		return 0, os.ErrClosed
	}
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case <-r.cancelChan:
		return 0, errors.New("stream cancelled: torrent closed or deleted")
	default:
	}

	return r.file.Seek(offset, whence)
}

func (r *directDiskReader) Close() error {
	if r.closed.Swap(true) {
		return nil
	}
	if r.onClose != nil {
		r.onClose()
	}
	return r.file.Close()
}

// emptySeeker provides seekable sizing for HEAD requests without opening descriptors or readers.
type emptySeeker struct {
	length int64
	offset int64
}

func newEmptySeeker(length int64) *emptySeeker {
	return &emptySeeker{length: length}
}

func (s *emptySeeker) Read(p []byte) (int, error) {
	return 0, io.EOF
}

func (s *emptySeeker) Seek(offset int64, whence int) (int64, error) {
	var newOffset int64
	switch whence {
	case io.SeekStart:
		newOffset = offset
	case io.SeekCurrent:
		newOffset = s.offset + offset
	case io.SeekEnd:
		newOffset = s.length + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if newOffset < 0 {
		return 0, errors.New("negative offset")
	}
	s.offset = newOffset
	return newOffset, nil
}

func (s *emptySeeker) Close() error {
	return nil
}

func isSafeRelativePath(rel string) bool {
	return payload.ValidateRelative(rel) == nil
}

// OpenStreamSource prepares a stream source for the given torrent and file.
func (e *Engine) OpenStreamSource(ctx context.Context, hash string, index int, isHEAD bool) (*StreamSource, error) {
	if e.ctx != nil && e.ctx.Err() != nil {
		return nil, e.ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	e.mu.RLock()
	mt, ok := e.managedTorrents[hash]
	e.mu.RUnlock()

	if !ok {
		return nil, TorrentNotFoundError{Hash: hash}
	}
	mt.mu.Lock()
	checking := mt.rechecking || mt.initializing || mt.verificationUncertain
	missing := mt.safetyBlocks[models.BlockMissingFiles] != ""
	mt.mu.Unlock()
	if checking {
		if missing {
			return nil, fs.ErrNotExist
		}
		return nil, ErrOperationConflict
	}
	if mt.t.Info() == nil {
		return nil, fmt.Errorf("torrent metadata is not available yet")
	}

	files := mt.t.Files()
	if index < 0 || index >= len(files) {
		return nil, fmt.Errorf("file index out of bounds: %d", index)
	}
	file := files[index]
	expected := mt.rememberExpectedPayload(index, file.CachedReadyForDirectRead())
	mt.t.RefreshStorageCompletion()
	if expected && !file.ReadyForDirectRead() {
		if err := e.detectMissingExpectedPayload(mt, file); err != nil {
			return nil, err
		}
	}
	displayName := file.DisplayPath()
	fileLength := file.Length()

	// If HEAD request, satisfy without torrent reader or OS descriptor
	if isHEAD {
		ready := file.ReadyForDirectRead()
		srcType := StreamSourceTorrent
		reason := FallbackReasonNotReady
		if ready {
			srcType = StreamSourceDisk
			reason = FallbackReasonNone
		}
		return &StreamSource{
			Type:           srcType,
			Reader:         newEmptySeeker(fileLength),
			Length:         fileLength,
			DisplayName:    displayName,
			FallbackReason: reason,
			cleanupFn:      func() error { return nil },
		}, nil
	}

	makeTorrentFallback := func(reason FallbackReason) (*StreamSource, error) {
		if err := e.CheckPayloadAdmission(hash); err != nil {
			return nil, err
		}
		logging.Debugf("stream fallback to torrent hash=%s file_index=%d reason=%s", hash, index, reason)
		readCtx, cancel := context.WithCancelCause(ctx)
		mt.mu.Lock()
		gateCtx := mt.payloadCtx
		mt.mu.Unlock()
		stop := func() bool { return true }
		if gateCtx != nil {
			stop = context.AfterFunc(gateCtx, func() { cancel(context.Cause(gateCtx)) })
		}
		reader := file.NewReader()
		reader.SetContext(readCtx)
		reader.SetReadahead(defaultStreamHeadPrebuffer)
		wrapped := &payloadStreamReader{ReadSeekCloser: reader, ctx: readCtx, cleanup: func() { stop(); cancel(context.Canceled) }, blockError: func() error {
			mt.mu.Lock()
			defer mt.mu.Unlock()
			if mt.downloadBlock != "" {
				return DownloadBlockedError{mt.downloadBlock}
			}
			return nil
		}}

		return &StreamSource{
			Type:           StreamSourceTorrent,
			Reader:         wrapped,
			Length:         fileLength,
			DisplayName:    displayName,
			FallbackReason: reason,
			cleanupFn:      wrapped.Close,
		}, nil
	}

	// 1. Check runtime readiness
	if !file.ReadyForDirectRead() {
		return makeTorrentFallback(FallbackReasonNotReady)
	}

	// 2. Storage check
	downloadDir := e.cfg.DownloadDir
	if downloadDir == "" {
		return makeTorrentFallback(FallbackReasonStorageUnsupported)
	}

	// 3. Compute relative path from layout
	info := mt.t.Info()
	fi := file.FileInfo()
	storageRoot, err := e.StorageRoot(hash)
	if err != nil {
		return nil, err
	}
	relPath := filepath.Join(storageRoot, payload.Path(info, &fi))
	if !isSafeRelativePath(relPath) {
		return makeTorrentFallback(FallbackReasonPathOutsideRoot)
	}

	// 4. Safe open within download directory
	root, err := os.OpenRoot(downloadDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if faultErr := e.detectMissingExpectedPayload(mt, file); faultErr != nil {
				return nil, faultErr
			}
		}
		logging.Warnf("stream open root failed path=%s: %v", downloadDir, err)
		return makeTorrentFallback(FallbackReasonOpenFailed)
	}
	defer root.Close()

	// Check if .part file still exists
	if _, err := root.Stat(relPath + ".part"); err == nil {
		return makeTorrentFallback(FallbackReasonPartFileExists)
	}

	f, err := payload.Open(root, relPath)
	if err != nil {
		if errors.Is(err, payload.ErrNotRegular) {
			return makeTorrentFallback(FallbackReasonNotRegularFile)
		}
		if errors.Is(err, fs.ErrNotExist) {
			if faultErr := e.detectMissingExpectedPayload(mt, file); faultErr != nil {
				return nil, faultErr
			}
			return makeTorrentFallback(FallbackReasonFinalFileNotFound)
		}
		logging.Warnf("stream direct file open failed rel_path=%s: %v", relPath, err)
		return makeTorrentFallback(FallbackReasonOpenFailed)
	}

	// 5. Stat on open descriptor
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return makeTorrentFallback(FallbackReasonOpenFailed)
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return makeTorrentFallback(FallbackReasonNotRegularFile)
	}
	if st.Size() != fileLength {
		_ = f.Close()
		logging.Warnf("stream direct file size mismatch expected=%d got=%d rel_path=%s", fileLength, st.Size(), relPath)
		return makeTorrentFallback(FallbackReasonSizeMismatch)
	}

	// 6. Re-check readiness after opening descriptor
	if !file.ReadyForDirectRead() {
		_ = f.Close()
		return makeTorrentFallback(FallbackReasonReadinessRevoked)
	}

	select {
	case <-mt.t.Closed():
		_ = f.Close()
		return nil, errors.New("torrent closed")
	default:
	}

	// 7. Register direct stream for lifecycle tracking
	var cancelChan <-chan struct{}
	var unregister func()
	if e.streamManager != nil {
		cancelChan, unregister, err = e.streamManager.RegisterDirectStream(hash, mt, f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
	}

	reader := &directDiskReader{
		file:       f,
		ctx:        ctx,
		cancelChan: cancelChan,
		onClose:    unregister,
	}

	logging.Infof("stream using direct disk source hash=%s file_index=%d path=%s size=%d", hash, index, relPath, fileLength)

	return &StreamSource{
		Type:           StreamSourceDisk,
		Reader:         reader,
		Length:         fileLength,
		DisplayName:    displayName,
		FallbackReason: FallbackReasonNone,
		cleanupFn:      reader.Close,
	}, nil
}

type payloadStreamReader struct {
	io.ReadSeekCloser
	ctx        context.Context
	cleanup    func()
	blockError func() error
}

func (r *payloadStreamReader) Read(p []byte) (int, error) {
	n, err := r.ReadSeekCloser.Read(p)
	if err != nil && context.Cause(r.ctx) != nil {
		err = context.Cause(r.ctx)
	} else if err != nil && r.blockError != nil {
		if blockErr := r.blockError(); blockErr != nil {
			err = blockErr
		}
	}
	return n, err
}

func (r *payloadStreamReader) Close() error { r.cleanup(); return r.ReadSeekCloser.Close() }

// CheckFileDirectReady verifies if the file is ready for direct disk reading without keeping a file handle open.
func (e *Engine) CheckFileDirectReady(hash string, index int) (bool, int64, error) {
	e.mu.RLock()
	mt, ok := e.managedTorrents[hash]
	e.mu.RUnlock()

	if !ok {
		return false, 0, TorrentNotFoundError{Hash: hash}
	}
	if mt.t.Info() == nil {
		return false, 0, fmt.Errorf("torrent metadata not available")
	}

	files := mt.t.Files()
	if index < 0 || index >= len(files) {
		return false, 0, fmt.Errorf("file index out of bounds: %d", index)
	}
	file := files[index]
	fileLength := file.Length()

	mt.mu.Lock()
	checking := mt.rechecking || mt.initializing || mt.verificationUncertain
	missing := mt.safetyBlocks[models.BlockMissingFiles] != ""
	mt.mu.Unlock()
	if checking {
		if missing {
			return false, fileLength, fs.ErrNotExist
		}
		return false, fileLength, nil
	}
	if !file.ReadyForDirectRead() {
		if mt.rememberExpectedPayload(index, false) {
			if err := e.detectMissingExpectedPayload(mt, file); err != nil {
				return false, fileLength, err
			}
		}
		return false, fileLength, nil
	}

	downloadDir := e.cfg.DownloadDir
	if downloadDir == "" {
		return false, fileLength, nil
	}

	info := mt.t.Info()
	fi := file.FileInfo()
	storageRoot, err := e.StorageRoot(hash)
	if err != nil {
		return false, fileLength, err
	}
	relPath := filepath.Join(storageRoot, payload.Path(info, &fi))
	if !isSafeRelativePath(relPath) {
		return false, fileLength, nil
	}

	root, err := os.OpenRoot(downloadDir)
	if err != nil {
		return false, fileLength, nil
	}
	defer root.Close()

	if _, err := root.Stat(relPath + ".part"); err == nil {
		return false, fileLength, nil
	}

	f, err := payload.Open(root, relPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if faultErr := e.detectMissingExpectedPayload(mt, file); faultErr != nil {
				return false, fileLength, faultErr
			}
		}
		return false, fileLength, nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() != fileLength {
		return false, fileLength, nil
	}

	if !file.ReadyForDirectRead() {
		return false, fileLength, nil
	}

	mt.rememberExpectedPayload(index, true)
	return true, fileLength, nil
}
