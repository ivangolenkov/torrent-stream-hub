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
	f, err := fst.fts.openSharedFile(file)
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
	if fst.fts.resume.hooks != nil && len(b) != 0 {
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
	defer unlock()
	if len(p) != 0 {
		if err := fst.fts.revokeResume(int(off/fst.fts.info.PieceLength), int((off+int64(len(p))-1)/fst.fts.info.PieceLength)+1); err != nil {
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
	for i, e := range fst.fts.segmentLocater.LocateIter(
		segments.Extent{Start: off, Length: int64(len(p))},
	) {
		if err := fst.fts.guardResumeFileWrite(i); err != nil {
			return n, err
		}
		var f fileWriter
		f, err = fst.fts.openForWrite(fst.fts.file(i))
		if err != nil {
			return
		}
		var n1 int
		var before os.FileInfo
		if statter, ok := f.(interface{ Stat() (os.FileInfo, error) }); ok {
			before, _ = statter.Stat()
		}
		n1, err = f.WriteAt(p[:e.Length], e.Start)
		if statter, ok := f.(interface{ Stat() (os.FileInfo, error) }); ok {
			after, _ := statter.Stat()
			fst.fts.acceptFileWrite(i, before, after, off+int64(n), int64(n1))
		}
		closeErr := f.Close()
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
