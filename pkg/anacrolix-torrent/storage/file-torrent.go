package storage

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"sync"

	"github.com/anacrolix/missinggo/v2"
	"github.com/anacrolix/missinggo/v2/panicif"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/segments"
)

type fileTorrentImpl struct {
	// Close is a filesystem-mutation barrier. A hash result may reach storage
	// after Torrent.Drop; it must not rename payload after application cleanup.
	lifecycle         sync.RWMutex
	closed            bool
	freshMu           sync.Mutex
	fresh             map[int][]os.FileInfo
	hashSnapshot      map[int][]os.FileInfo
	info              *metainfo.Info
	files             []fileExtra
	metainfoFileInfos []metainfo.FileInfo
	segmentLocater    segments.Index
	infoHash          metainfo.Hash
	io                fileIo
	// Save memory by pointing to the other data.
	client *fileClientImpl
}

func (fts *fileTorrentImpl) logger() *slog.Logger {
	return fts.client.opts.Logger
}

func (fts *fileTorrentImpl) pieceCompletion() PieceCompletion {
	return fts.client.opts.PieceCompletion
}

func (fts *fileTorrentImpl) pieceCompletionKey(p int) metainfo.PieceKey {
	return metainfo.PieceKey{
		InfoHash: fts.infoHash,
		Index:    p,
	}
}

func (fts *fileTorrentImpl) setPieceCompletion(p int, complete bool) error {
	return fts.pieceCompletion().Set(fts.pieceCompletionKey(p), complete)
}

// Set piece completions based on whether all files in each piece are not .part files.
func (fts *fileTorrentImpl) setCompletionFromPartFiles() error {
	notComplete := make([]bool, fts.info.NumPieces())
	for fileIndex := range fts.files {
		f := fts.file(fileIndex)
		fi, err := os.Stat(f.safeOsPath)
		if err == nil {
			if fi.Size() == f.length() {
				continue
			}
			fts.logger().Warn("file has unexpected size", "file", f.safeOsPath, "size", fi.Size(), "expected", f.length())
		} else if !errors.Is(err, fs.ErrNotExist) {
			fts.logger().Warn("error checking file size", "err", err)
		}
		// Ensure all pieces associated with a file are not marked as complete (at most unknown).
		for pieceIndex := f.beginPieceIndex(); pieceIndex < f.endPieceIndex(); pieceIndex++ {
			notComplete[pieceIndex] = true
		}
	}
	for i, nc := range notComplete {
		if nc {
			c := fts.getCompletion(i)
			if c.Complete {
				// TODO: We need to set unknown so that verification of the data we do have could
				// occur naturally but that'll be a big change.
				panicif.Err(fts.setPieceCompletion(i, false))
			}
		} else {
			err := fts.setPieceCompletion(i, true)
			if err != nil {
				return fmt.Errorf("setting piece %v completion: %w", i, err)
			}
		}
	}
	return nil
}

func (fts *fileTorrentImpl) partFiles() bool {
	return fts.client.opts.partFiles()
}

func (fts *fileTorrentImpl) pathForWrite(f *file) string {
	if fts.partFiles() {
		return f.partFilePath()
	}
	return f.safeOsPath
}

func (fts *fileTorrentImpl) getCompletion(piece int) Completion {
	if fts.client.opts.VerifyOnOpen {
		return fts.freshCompletion(piece)
	}
	cmpl, err := fts.pieceCompletion().Get(metainfo.PieceKey{fts.infoHash, piece})
	cmpl.Err = errors.Join(cmpl.Err, err)
	return cmpl
}

func (fts *fileTorrentImpl) Piece(p metainfo.Piece) PieceImpl {
	// Create a view onto the file-based torrent storage.
	_io := fileTorrentImplIO{fts}
	// Return the appropriate segments of this.
	return &filePieceImpl{
		fts,
		p,
		missinggo.NewSectionWriter(_io, p.Offset(), p.Length()),
		io.NewSectionReader(_io, p.Offset(), p.Length()),
	}
}

func (fts *fileTorrentImpl) stat(name string) (os.FileInfo, error) {
	if r, ok := fts.io.(rootFileIo); ok {
		return r.stat(name)
	}
	return os.Stat(name)
}

func (fts *fileTorrentImpl) pieceSignature(index int) ([]os.FileInfo, error) {
	var result []os.FileInfo
	p := fts.info.Piece(index)
	for i := range fts.segmentLocater.LocateIter(segments.Extent{p.Offset(), p.Length()}) {
		f := fts.file(i)
		f.mu.RLock()
		var st os.FileInfo
		var err error
		if fts.partFiles() {
			st, err = fts.stat(f.partFilePath())
		}
		if st == nil && (err == nil || errors.Is(err, os.ErrNotExist)) {
			st, err = fts.stat(f.safeOsPath)
		}
		f.mu.RUnlock()
		if err != nil {
			return nil, err
		}
		result = append(result, st)
	}
	return result, nil
}

func (fts *fileTorrentImpl) freshCompletion(index int) Completion {
	fts.freshMu.Lock()
	defer fts.freshMu.Unlock()
	old, ok := fts.fresh[index]
	if !ok {
		return Completion{}
	}
	current, err := fts.pieceSignature(index)
	if err != nil || len(old) != len(current) {
		delete(fts.fresh, index)
		return Completion{}
	}
	for i, st := range old {
		other := current[i]
		if !os.SameFile(st, other) || st.Size() != other.Size() || !st.ModTime().Equal(other.ModTime()) {
			delete(fts.fresh, index)
			return Completion{}
		}
	}
	return Completion{Ok: true, Complete: true}
}

// Internal writes to a missing piece do not invalidate unrelated verified
// pieces merely because the containing file's mtime/size changed.
func (fts *fileTorrentImpl) acceptWrite(before, after os.FileInfo, off, n int64) {
	if !fts.client.opts.VerifyOnOpen || before == nil || after == nil {
		return
	}
	fts.freshMu.Lock()
	defer fts.freshMu.Unlock()
	for _, cache := range []map[int][]os.FileInfo{fts.fresh, fts.hashSnapshot} {
		for piece, signature := range cache {
			start := int64(piece) * fts.info.PieceLength
			end := start + fts.info.PieceLength
			if n > 0 && off < end && off+n > start {
				delete(cache, piece)
				continue
			}
			for i, old := range signature {
				if os.SameFile(old, before) && old.Size() == before.Size() && old.ModTime().Equal(before.ModTime()) {
					signature[i] = after
				}
			}
		}
	}
}

func (fs *fileTorrentImpl) Close() error {
	fs.lifecycle.Lock()
	defer fs.lifecycle.Unlock()
	fs.closed = true
	return nil
}

func (fs *fileTorrentImpl) beginOperation() bool {
	fs.lifecycle.RLock()
	if fs.closed {
		fs.lifecycle.RUnlock()
		return false
	}
	return true
}

func (fts *fileTorrentImpl) file(index int) file {
	return file{
		Info:      fts.info,
		FileInfo:  &fts.metainfoFileInfos[index],
		fileExtra: &fts.files[index],
	}
}

// Open file for reading.
func (me *fileTorrentImpl) openSharedFile(file file) (f sharedFileIf, err error) {
	file.mu.RLock()
	// Fine to open once under each name on a unix system. We could make the shared file keys more
	// constrained, but it shouldn't matter. TODO: Ensure at most one of the names exist.
	if me.partFiles() {
		f, err = me.io.openForSharedRead(file.partFilePath())
	}
	if err == nil && f == nil || errors.Is(err, fs.ErrNotExist) {
		f, err = me.io.openForSharedRead(file.safeOsPath)
	}
	file.mu.RUnlock()
	return
}

// Open file for reading. Not a shared handle if that matters.
func (me *fileTorrentImpl) openFile(file file) (f fileReader, err error) {
	file.mu.RLock()
	// Fine to open once under each name on a unix system. We could make the shared file keys more
	// constrained, but it shouldn't matter. TODO: Ensure at most one of the names exist.
	if me.partFiles() {
		f, err = me.io.openForRead(file.partFilePath())
	}
	if err == nil && f == nil || errors.Is(err, fs.ErrNotExist) {
		f, err = me.io.openForRead(file.safeOsPath)
	}
	file.mu.RUnlock()
	return
}

func (fst *fileTorrentImpl) openForWrite(file file) (_ fileWriter, err error) {
	// It might be possible to have a writable handle shared files cache if we need it.
	fst.logger().Debug("openForWrite", "file.safeOsPath", file.safeOsPath)
	return fst.io.openForWrite(fst.pathForWrite(&file), file.FileInfo.Length)
}
