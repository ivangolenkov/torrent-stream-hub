package storage

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"

	"github.com/anacrolix/missinggo/v2/panicif"

	"github.com/anacrolix/torrent/segments"
)

// Exposes file-based storage of a torrent, as one big ReadWriterAt.
type fileTorrentImplIO struct {
	fts *fileTorrentImpl
}

// Returns EOF on short or missing file.
func (fst fileTorrentImplIO) readFileAt(file file, b []byte, off int64) (n int, err error) {
	fst.fts.logger().Debug("readFileAt", "file.safeOsPath", file.safeOsPath)
	var f sharedFileIf
	if fst.fts.managedPayload() {
		f, err = fst.fts.acquireManagedFile(file.index, false)
	} else {
		f, err = fst.fts.openSharedFile(file)
	}
	if errors.Is(err, fs.ErrNotExist) {
		// File missing is treated the same as a short file. Should we propagate this through the
		// interface now that fs.ErrNotExist is a thing?
		err = io.EOF
		return
	}
	if err != nil {
		return
	}
	defer f.Close()
	// Limit the read to within the expected bounds of this file.
	if int64(len(b)) > file.length()-off {
		b = b[:file.length()-off]
	}
	for off < file.length() && len(b) != 0 {
		n1, err1 := f.ReadAt(b, off)
		b = b[n1:]
		n += n1
		off += int64(n1)
		if n1 == 0 {
			err = err1
			break
		}
	}
	return
}

// Only returns EOF at the end of the torrent. Premature EOF is ErrUnexpectedEOF.
func (fst fileTorrentImplIO) ReadAt(b []byte, off int64) (n int, err error) {
	if fst.fts.readClosed.Load() {
		return 0, os.ErrClosed
	}
	if off < 0 || int64(len(b)) > math.MaxInt64-off {
		return 0, io.EOF
	}
	unlock := fst.fts.lockPayloadRange(off, int64(len(b)), false)
	defer unlock()
	if fst.fts.readClosed.Load() {
		return 0, os.ErrClosed
	}
	if (fst.fts.resume.hooks != nil || fst.fts.managedPayload()) && len(b) != 0 {
		if off < 0 || int64(len(b)) > math.MaxInt64-off || fst.fts.info.PieceLength <= 0 || (off+int64(len(b))-1)/fst.fts.info.PieceLength >= int64(fst.fts.info.NumPieces()) {
			return 0, io.EOF
		}
		for i, end := int(off/fst.fts.info.PieceLength), int((off+int64(len(b))-1)/fst.fts.info.PieceLength)+1; i < end; i++ {
			if !fst.fts.cachedCompletion(i).Complete {
				return 0, ErrUnverifiedPayload
			}
		}
	}
	for i, e := range fst.fts.segmentLocater.LocateIter(
		segments.Extent{Start: off, Length: int64(len(b))},
	) {
		n1, err1 := fst.readFileAt(fst.fts.file(i), b[:e.Length], e.Start)
		cancelled := errors.Is(err1, context.Canceled) || errors.Is(err1, context.DeadlineExceeded)
		if !cancelled && ((err1 != nil && err1 != io.EOF) || segments.Int(n1) != e.Length) {
			if revokeErr := fst.fts.revokeResumeFile(i); revokeErr != nil {
				return n + n1, errors.Join(err1, revokeErr)
			}
		}
		n += n1
		b = b[n1:]
		if segments.Int(n1) == e.Length {
			switch err1 {
			// ReaderAt.ReadAt contract.
			case nil, io.EOF:
			default:
				err = err1
				return
			}
		} else {
			panicif.Nil(err1)
			err = err1
			return
		}
	}
	if len(b) != 0 {
		// We're at the end of the torrent.
		err = io.EOF
	}
	return
}

func (fst fileTorrentImplIO) WriteAt(p []byte, off int64) (n int, err error) {
	if !fst.fts.beginOperation() {
		return 0, os.ErrClosed
	}
	defer fst.fts.lifecycle.RUnlock()
	if len(p) != 0 && (off < 0 || int64(len(p)) > math.MaxInt64-off || fst.fts.info.PieceLength <= 0 || (off+int64(len(p))-1)/fst.fts.info.PieceLength >= int64(fst.fts.info.NumPieces())) {
		return 0, io.ErrShortWrite
	}
	unlock := fst.fts.lockPayloadRange(off, int64(len(p)), true)
	timings := &fst.fts.ioTimings
	holdStart := timings.writeLeaseHold.begin()
	release := func() {
		if unlock != nil {
			timings.writeLeaseHold.end(holdStart, nil)
			unlock()
			unlock = nil
		}
	}
	defer release()
	prepareStart := timings.writePrepare.begin()
	if len(p) != 0 {
		if err := fst.fts.revokeResume(int(off/fst.fts.info.PieceLength), int((off+int64(len(p))-1)/fst.fts.info.PieceLength)+1); err != nil {
			timings.writePrepare.end(prepareStart, err)
			return 0, err
		}
		fst.fts.freshMu.Lock()
		for i, end := int(off/fst.fts.info.PieceLength), int((off+int64(len(p))-1)/fst.fts.info.PieceLength)+1; i < end; i++ {
			fst.fts.forgetPieceLocked(i)
			delete(fst.fts.hashSnapshot, i)
		}
		fst.fts.freshMu.Unlock()
	}
	fst.fts.checkpointEpoch.Add(1)
	timings.writePrepare.end(prepareStart, nil)
	for i, e := range fst.fts.segmentLocater.LocateIter(
		segments.Extent{Start: off, Length: int64(len(p))},
	) {
		if fst.fts.managedPayload() {
			start := timings.writeOpen.begin()
			lease, openErr := fst.fts.acquireManagedFile(i, true)
			timings.writeOpen.end(start, openErr)
			if openErr != nil {
				return n, openErr
			}
			start = timings.writePayload.begin()
			n1, writeErr := lease.entry.file.WriteAt(p[:e.Length], e.Start)
			if writeErr == nil && int64(n1) != e.Length {
				writeErr = io.ErrShortWrite
			}
			timings.writePayload.end(start, writeErr)
			if writeErr != nil {
				lease.invalidate()
				writeErr = errors.Join(writeErr, fst.fts.revokeResumeFile(i))
			}
			start = timings.writeClose.begin()
			closeErr := lease.Close()
			timings.writeClose.end(start, closeErr)
			n += n1
			p = p[n1:]
			if err := errors.Join(writeErr, closeErr); err != nil {
				return n, err
			}
			continue
		}
		start := timings.writeGuard.begin()
		guardErr := fst.fts.guardResumeFileWrite(i)
		timings.writeGuard.end(start, guardErr)
		if guardErr != nil {
			return n, guardErr
		}
		var f fileWriter
		start = timings.writeOpen.begin()
		f, err = fst.fts.openForWrite(fst.fts.file(i))
		timings.writeOpen.end(start, err)
		if err != nil {
			return
		}
		var n1 int
		var before os.FileInfo
		if statter, ok := f.(interface{ Stat() (os.FileInfo, error) }); ok {
			start = timings.writeStat.begin()
			var statErr error
			before, statErr = statter.Stat()
			timings.writeStat.end(start, statErr)
		}
		start = timings.writePayload.begin()
		n1, err = f.WriteAt(p[:e.Length], e.Start)
		writeErr := err
		if writeErr == nil && int64(n1) != e.Length {
			writeErr = io.ErrShortWrite
		}
		timings.writePayload.end(start, writeErr)
		if statter, ok := f.(interface{ Stat() (os.FileInfo, error) }); ok {
			start = timings.writeStat.begin()
			after, statErr := statter.Stat()
			timings.writeStat.end(start, statErr)
			start = timings.writeAccount.begin()
			fst.fts.acceptFileWrite(i, before, after, off+int64(n), int64(n1))
			timings.writeAccount.end(start, nil)
		}
		// All payload mutation and before/after accounting remain exclusive.
		// Only the final descriptor may close outside the lease: no later
		// extent can then mutate a range that a verifier has just observed.
		// Part-file promotion and legacy verification retain the old scope.
		closer, independent := f.(payloadIndependentCloser)
		if independent && fst.fts.client.opts.VerifyOnOpen && !fst.fts.partFiles() && e.Length == int64(len(p)) {
			release()
		}
		start = timings.writeClose.begin()
		var closeErr error
		if independent {
			closeErr = closer.closeWithoutPayloadMutation()
		} else {
			closeErr = f.Close()
		}
		timings.writeClose.end(start, closeErr)
		n += n1
		p = p[n1:]
		if err == nil {
			err = closeErr
		}
		if err == nil && int64(n1) != e.Length {
			err = io.ErrShortWrite
		}
		if err != nil {
			return
		}
	}
	return
}
