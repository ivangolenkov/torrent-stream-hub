package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"

	"github.com/anacrolix/missinggo/v2"
	"github.com/anacrolix/missinggo/v2/panicif"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/segments"
)

type fileTorrentImpl struct {
	// Close is a filesystem-mutation barrier. A hash result may reach storage
	// after Torrent.Drop; it must not rename payload after application cleanup.
	lifecycle         sync.RWMutex
	resume            resumeMutationState
	closed            bool
	readClosed        atomic.Bool // reads use payload leases, not the seal mutation gate
	directory         string
	checkpointEpoch   atomic.Uint64
	checkpointSealed  atomic.Uint64
	closeErr          error
	freshMu           sync.Mutex
	fresh             map[int]pieceEvidence
	hashSnapshot      map[int][]os.FileInfo
	signatures        map[int]*verifiedFileSignature
	statHook          func()
	checkpointSyncDir func(*os.Root, string) error // nil uses native directory sync
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
	cmpl, err := fts.pieceCompletion().Get(metainfo.PieceKey{InfoHash: fts.infoHash, Index: piece})
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
	if fts.statHook != nil {
		fts.statHook()
	}
	if r, ok := fts.io.(rootFileIo); ok {
		return r.stat(name)
	}
	return os.Stat(name)
}

func (fts *fileTorrentImpl) pieceSignature(index int) ([]os.FileInfo, error) {
	var result []os.FileInfo
	p := fts.info.Piece(index)
	for i := range fts.segmentLocater.LocateIter(segments.Extent{Start: p.Offset(), Length: p.Length()}) {
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

// All signature fields and references are protected by freshMu. Internal writes
// update one shared signature; no loop over previously verified pieces is needed.
type verifiedFileSignature struct {
	os.FileInfo
	file      int
	valid     bool
	complete  int
	published bool
}

func sameSignature(a, b os.FileInfo) bool {
	if v, ok := a.(*verifiedFileSignature); ok {
		a = v.FileInfo
	}
	if v, ok := b.(*verifiedFileSignature); ok {
		b = v.FileInfo
	}
	return a != nil && b != nil && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func (fts *fileTorrentImpl) internSignatureLocked(index int, current []os.FileInfo) []os.FileInfo {
	if fts.signatures == nil {
		fts.signatures = make(map[int]*verifiedFileSignature)
	}
	result := make([]os.FileInfo, 0, len(current))
	n := 0
	p := fts.info.Piece(index)
	for file := range fts.segmentLocater.LocateIter(segments.Extent{Start: p.Offset(), Length: p.Length()}) {
		st := current[n]
		n++
		signature := fts.signatures[file]
		if signature == nil || !signature.valid || !sameSignature(signature.FileInfo, st) {
			if signature != nil {
				signature.valid = false
			}
			signature = &verifiedFileSignature{FileInfo: st, file: file, valid: true}
			fts.signatures[file] = signature
		}
		result = append(result, signature)
	}
	return result
}

type pieceEvidence struct {
	infos    []os.FileInfo
	restored bool
}

func (fts *fileTorrentImpl) forgetPieceLocked(index int) {
	for _, info := range fts.fresh[index].infos {
		if sig, ok := info.(*verifiedFileSignature); ok && sig.valid {
			sig.complete--
			sig.published = false
		}
	}
	delete(fts.fresh, index)
}
func (fts *fileTorrentImpl) markFreshLocked(index int, signature []os.FileInfo, restored bool) {
	old, ok := fts.fresh[index]
	equal := ok && len(old.infos) == len(signature) && fts.cachedCompletionLocked(index).Complete
	if equal {
		for i := range old.infos {
			if old.infos[i] != signature[i] {
				equal = false
				break
			}
		}
	}
	if equal {
		old.restored = restored
		fts.fresh[index] = old
		delete(fts.hashSnapshot, index)
		return
	}
	fts.forgetPieceLocked(index)
	fts.fresh[index] = pieceEvidence{infos: signature, restored: restored}
	for _, info := range signature {
		if sig, ok := info.(*verifiedFileSignature); ok {
			sig.complete++
		}
	}
	delete(fts.hashSnapshot, index)
}
func (fts *fileTorrentImpl) cachedCompletionLocked(index int) Completion {
	signature, ok := fts.fresh[index]
	if !ok {
		return Completion{}
	}
	for _, info := range signature.infos {
		if sig, ok := info.(*verifiedFileSignature); ok && !sig.valid {
			return Completion{}
		}
	}
	return Completion{Ok: true, Complete: true}
}
func (fts *fileTorrentImpl) cachedCompletion(index int) Completion {
	if fts.readClosed.Load() {
		return Completion{}
	}
	fts.freshMu.Lock()
	defer fts.freshMu.Unlock()
	return fts.cachedCompletionLocked(index)
}

// Refresh filesystem evidence without holding freshMu (or the library client
// lock). A concurrent internal write changes the captured signature and makes an
// older stat result inapplicable, rather than invalidating unrelated pieces.
func (fts *fileTorrentImpl) refreshFile(index int) {
	if fts.readClosed.Load() {
		return
	}
	fts.files[index].payloadMu.RLock()
	defer fts.files[index].payloadMu.RUnlock()
	if fts.readClosed.Load() {
		return
	}
	fts.freshMu.Lock()
	sig := fts.signatures[index]
	if sig == nil || !sig.valid {
		fts.freshMu.Unlock()
		return
	}
	before := sig.FileInfo
	fts.freshMu.Unlock()
	f := fts.file(index)
	f.mu.RLock()
	var current os.FileInfo
	var err error
	if fts.partFiles() {
		current, err = fts.stat(f.partFilePath())
	}
	if current == nil && (err == nil || errors.Is(err, os.ErrNotExist)) {
		current, err = fts.stat(f.safeOsPath)
	}
	f.mu.RUnlock()
	fts.freshMu.Lock()
	if fts.signatures[index] != sig || !sameSignature(before, sig.FileInfo) {
		fts.freshMu.Unlock()
		return
	}
	invalid := err != nil || !sameSignature(before, current)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		invalid = false
	}
	if invalid {
		fts.checkpointEpoch.Add(1)
		sig.valid = false
		sig.complete = 0
		sig.published = false
	}
	fts.freshMu.Unlock()
	if invalid {
		_ = fts.revokeResumeFile(index)
	} // live gate remains closed even if durability fails
}
func (fts *fileTorrentImpl) refreshRange(begin, end int) {
	if begin >= end {
		return
	}
	for index := range fts.segmentLocater.LocateIter(segments.Extent{Start: int64(begin) * fts.info.PieceLength, Length: int64(end-begin) * fts.info.PieceLength}) {
		fts.refreshFile(index)
	}
}
func (fts *fileTorrentImpl) freshCompletion(index int) Completion {
	if !fts.cachedCompletion(index).Complete {
		return Completion{}
	}
	fts.refreshRange(index, index+1)
	return fts.cachedCompletion(index)
}

func (fts *fileTorrentImpl) acceptWrite(before, after os.FileInfo, off, n int64) {
	if !fts.client.opts.VerifyOnOpen || before == nil || after == nil || n <= 0 {
		return
	}
	for index := range fts.segmentLocater.LocateIter(segments.Extent{Start: off, Length: n}) {
		fts.acceptFileWrite(index, before, after, off, n)
	}
}
func (fts *fileTorrentImpl) acceptFileWrite(index int, before, after os.FileInfo, off, n int64) {
	if !fts.client.opts.VerifyOnOpen || before == nil || after == nil || n <= 0 {
		return
	}
	fts.freshMu.Lock()
	defer fts.freshMu.Unlock()
	if sig := fts.signatures[index]; sig != nil && sig.valid {
		if sameSignature(sig.FileInfo, before) && os.SameFile(before, after) {
			sig.FileInfo = after
		} else {
			sig.valid = false
			sig.complete = 0
			sig.published = false
		}
	}
	for piece := int(off / fts.info.PieceLength); piece <= int((off+n-1)/fts.info.PieceLength); piece++ {
		fts.forgetPieceLocked(piece)
		delete(fts.hashSnapshot, piece)
	}
}

func (fs *fileTorrentImpl) Close() error {
	fs.lifecycle.Lock()
	defer fs.lifecycle.Unlock()
	if fs.closed {
		return fs.closeErr
	}
	fs.closed = true
	fs.readClosed.Store(true)
	for i := range fs.files {
		fs.files[i].payloadMu.Lock()
	}
	defer func() {
		for i := len(fs.files) - 1; i >= 0; i-- {
			fs.files[i].payloadMu.Unlock()
		}
	}()
	fs.freshMu.Lock()
	clear(fs.fresh)
	clear(fs.hashSnapshot)
	for _, sig := range fs.signatures {
		sig.valid = false
		sig.complete = 0
		sig.published = false
	}
	for i := range fs.files {
		fs.files[i].zeroReady = false
	}
	fs.freshMu.Unlock()
	// Per-torrent backends may retain handles after an individual read/write.
	// RootFS remains owned by fileClientImpl, not by a torrent's I/O adapter.
	if closer, ok := fs.io.(io.Closer); ok {
		fs.closeErr = closer.Close()
	}
	return fs.closeErr
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
