package storage

import (
	"bytes"
	"context"
	"errors"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/segments"
)

var (
	ErrCheckpointUnsupported = errors.New("storage checkpoint unsupported")
	ErrCheckpointChanged     = errors.New("storage checkpoint changed or revoked")
	ErrCheckpointBudget      = errors.New("storage checkpoint budget exceeded")
)

const (
	checkpointMaxFiles     = 4096
	checkpointMaxPieces    = 4 << 20
	checkpointMaxPaths     = 1 << 20
	checkpointWorkingBytes = 32 << 20
)

// FileCheckpoint contains no persistent authority. The application must bind it
// to record/mapping/volume/generation and commit only from WithCheckpoint's callback.
// File paths are relative to Root; Root itself is relative to the managed RootFS.
type FileCheckpoint struct {
	InfoHash     metainfo.Hash
	Root         string
	PieceLength  int64
	PieceCount   int
	ExtentLength int64
	Files        []CheckpointFile
	Complete     []byte
}

type CheckpointFile struct {
	Path             string
	Offset, Length   int64
	Present          bool
	Identity         string
	Size, ModifiedNS int64
}

// RestoreToken can only be created by PrepareRestore after rooted evidence checks.
// It is one-use, scoped to one backend open and invalidated by verification/write
// activity. It does not replace the application's durable owner/attachment fence.
type RestoreToken struct {
	owner *fileTorrentImpl
	ctx   context.Context
	epoch uint64
	used  atomic.Bool
	check func(context.Context) error
	proof FileCheckpoint
	infos []os.FileInfo
}

func validCheckpointBits(mask []byte, count int) bool {
	return count >= 0 && count <= checkpointMaxPieces && len(mask) == (count+7)/8 &&
		(count%8 == 0 || mask[len(mask)-1]>>uint(count%8) == 0)
}

func checkpointBit(mask []byte, index int) bool { return mask[index/8]&(1<<uint(index%8)) != 0 }

func clearCheckpointFile(mask []byte, f CheckpointFile, pieceLength int64) {
	if f.Length == 0 {
		return
	}
	for i, end := f.Offset/pieceLength, (f.Offset+f.Length-1)/pieceLength; i <= end; i++ {
		mask[i/8] &^= 1 << uint(i%8)
	}
}

func checkpointFileWanted(mask []byte, f CheckpointFile, pieceLength int64) bool {
	if f.Length == 0 {
		return f.Present
	}
	for i, end := f.Offset/pieceLength, (f.Offset+f.Length-1)/pieceLength; i <= end; i++ {
		if checkpointBit(mask, int(i)) {
			return true
		}
	}
	return false
}

// TryLock polling is bounded/cancelable and spawns no waiter goroutine. Once an
// I/O syscall is issued its owner retains the lock until that syscall returns.
func (t *fileTorrentImpl) checkpointLock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.lifecycle.TryLock() {
		if t.closed {
			t.lifecycle.Unlock()
			return os.ErrClosed
		}
		return nil
	}
	timer := time.NewTicker(time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			if t.lifecycle.TryLock() {
				if t.closed {
					t.lifecycle.Unlock()
					return os.ErrClosed
				}
				return nil
			}
		}
	}
}

func (t *fileTorrentImpl) checkpointCatalog() (FileCheckpoint, error) {
	var c FileCheckpoint
	if t.client.opts.RootFS == nil || !t.client.opts.VerifyOnOpen || t.partFiles() || t.info.PieceLength <= 0 {
		return c, ErrCheckpointUnsupported
	}
	if len(t.files) > checkpointMaxFiles {
		return c, ErrCheckpointBudget
	}
	c.InfoHash, c.PieceLength, c.PieceCount = t.infoHash, t.info.PieceLength, t.info.NumPieces()
	if c.PieceCount < 0 || c.PieceCount > checkpointMaxPieces {
		return c, ErrCheckpointBudget
	}
	var err error
	c.Root, err = filepath.Rel(t.client.opts.RootFS.Name(), t.directory)
	if err != nil || !filepath.IsLocal(c.Root) || c.Root == "." {
		return c, ErrCheckpointUnsupported
	}
	paths := 0
	for i := range t.files {
		f := t.file(i)
		path, err := filepath.Rel(t.directory, f.safeOsPath)
		if err != nil || !filepath.IsLocal(path) || path == "." {
			return c, ErrUnsafeFile
		}
		paths += len(c.Root) + len(path) + 1
		if paths > checkpointMaxPaths || len(path)+len(c.Root)+1 > 4096 {
			return c, ErrCheckpointBudget
		}
		if f.TorrentOffset < 0 || f.length() < 0 || f.length() > math.MaxInt64-f.TorrentOffset {
			return c, ErrCheckpointUnsupported
		}
		c.Files = append(c.Files, CheckpointFile{Path: path, Offset: f.TorrentOffset, Length: f.length()})
		c.ExtentLength = max(c.ExtentLength, f.TorrentOffset+f.length())
	}
	count := c.ExtentLength / c.PieceLength
	if c.ExtentLength%c.PieceLength != 0 {
		count++
	}
	if count != int64(c.PieceCount) {
		return c, ErrCheckpointUnsupported
	}
	return c, nil
}

func (t *fileTorrentImpl) checkpointEvidence(ctx context.Context) (FileCheckpoint, []os.FileInfo, error) {
	c, err := t.checkpointCatalog()
	if err != nil {
		return c, nil, err
	}
	infos := make([]os.FileInfo, len(c.Files))
	r := rootFileIo{root: t.client.opts.RootFS, guard: t.client.opts.IOGuard, onEvent: t.client.opts.IOEvent, ctx: ctx, readOnly: true}
	for i := range c.Files {
		if err := ctx.Err(); err != nil {
			return c, nil, err
		}
		f, err := r.open(t.files[i].safeOsPath, os.O_RDONLY)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return c, nil, err
		}
		info, statErr := f.Stat()
		id, idErr := FileIdentity(f)
		err = errors.Join(statErr, idErr, f.Close())
		if err != nil {
			return c, nil, err
		}
		infos[i] = info
		c.Files[i].Present, c.Files[i].Identity = true, id
		c.Files[i].Size, c.Files[i].ModifiedNS = info.Size(), info.ModTime().UnixNano()
	}
	return c, infos, nil
}

// withCheckpoint joins storage mutations and marking, flushes the stable subset,
// then holds mutation admission closed throughout publish. Reads need no lifecycle
// lock and continue; callbacks must not reenter this backend or the library lock.
func (t *fileTorrentImpl) withCheckpoint(ctx context.Context, eligible []byte, publish func(FileCheckpoint) error) error {
	if publish == nil || !validCheckpointBits(eligible, t.info.NumPieces()) {
		return ErrCheckpointUnsupported
	}
	if err := t.checkpointLock(ctx); err != nil {
		return err
	}
	defer t.lifecycle.Unlock()
	event := func(operation, path string) {
		if observe := t.client.opts.IOEvent; observe != nil {
			observe(operation, path)
		}
	}
	event("checkpoint-snapshot-before", "")
	revision := t.checkpointEpoch.Load()
	c, infos, err := t.checkpointEvidence(ctx)
	if err != nil {
		return err
	}
	c.Complete = bytes.Clone(eligible)
	t.freshMu.Lock()
	for i, current := range infos {
		if sig := t.signatures[i]; sig != nil && sig.valid && !sameSignature(sig.FileInfo, current) {
			sig.valid, sig.complete, sig.published = false, 0, false
		}
	}
	for i := 0; i < c.PieceCount; i++ {
		_, hashing := t.hashSnapshot[i]
		if hashing || !t.cachedCompletionLocked(i).Complete {
			c.Complete[i/8] &^= 1 << uint(i%8)
		}
	}
	t.freshMu.Unlock()
	for _, f := range c.Files {
		if !f.Present || f.Size < f.Length {
			clearCheckpointFile(c.Complete, f, c.PieceLength)
		}
	}
	event("checkpoint-snapshot-after", "")
	dirs := make(map[string]struct{})
	flushIO := t.io
	if rooted, ok := flushIO.(rootFileIo); ok {
		rooted.ctx = ctx
		flushIO = rooted
	}
	for i, f := range c.Files {
		if !checkpointFileWanted(c.Complete, f, c.PieceLength) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(c.Root, f.Path)
		event("checkpoint-payload-sync-before", path)
		if err := flushIO.flush(t.files[i].safeOsPath, 0, f.Length); err != nil {
			return err
		}
		event("checkpoint-payload-sync-after", path)
		for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
			dirs[dir] = struct{}{}
			if dir == "." {
				break
			}
		}
	}
	for dir := range dirs {
		if err := ctx.Err(); err != nil {
			return err
		}
		syncDir := t.checkpointSyncDir
		if syncDir == nil {
			syncDir = SyncRootDirectory
		}
		event("checkpoint-directory-sync-before", dir)
		if err := syncDir(t.client.opts.RootFS, dir); err != nil {
			return err
		}
		event("checkpoint-directory-sync-after", dir)
	}
	after, _, err := t.checkpointEvidence(ctx)
	if err != nil {
		return err
	}
	for i := range c.Files {
		if c.Files[i] != after.Files[i] {
			return ErrCheckpointChanged
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A commit can succeed despite a lost/cancelled acknowledgement. Preserve
	// the union before invoking publish, so subsequent writes still revoke it.
	t.rememberResume(c.Complete, false)
	if err := publish(c); err != nil {
		return err
	}
	t.rememberResume(c.Complete, true)
	t.checkpointSealed.Store(revision)
	return ctx.Err()
}

// checkpointState is cached scheduling metadata, never a readiness probe. It
// performs no filesystem or SQL work and may be sampled by a bounded scanner.
func (t *fileTorrentImpl) checkpointState() (uint64, bool) {
	revision := t.checkpointEpoch.Load()
	return revision, !t.readClosed.Load() && revision != t.checkpointSealed.Load()
}

func sameCheckpointCatalog(a, b FileCheckpoint) bool {
	if a.InfoHash != b.InfoHash || a.Root != b.Root || a.PieceLength != b.PieceLength || a.PieceCount != b.PieceCount || a.ExtentLength != b.ExtentLength || len(a.Files) != len(b.Files) {
		return false
	}
	for i, f := range a.Files {
		n := b.Files[i]
		if f.Path != n.Path || f.Offset != n.Offset || f.Length != n.Length {
			return false
		}
	}
	return true
}

func checkpointImportBudget(c FileCheckpoint) bool {
	n := 0
	for _, b := range c.Complete {
		n += bits.OnesCount8(b)
	}
	// Conservative overhead for map buckets, per-piece interfaces/slices and
	// shared file signatures; do not duplicate the whole library's piece array.
	// Include claims/proof, factory/backend pending masks and revoke scratch,
	// not only the two transient masks used by the read-only implementation.
	return int64(n)*256+int64(len(c.Files))*2048+int64(len(c.Complete))*8 <= checkpointWorkingBytes
}

func (t *fileTorrentImpl) checkpointContributions(c FileCheckpoint, infos []os.FileInfo) error {
	for piece := 0; piece < c.PieceCount; piece++ {
		if !checkpointBit(c.Complete, piece) {
			continue
		}
		off := int64(piece) * c.PieceLength
		found := false
		for i := range t.segmentLocater.LocateIter(segments.Extent{Start: off, Length: min(c.PieceLength, c.ExtentLength-off)}) {
			if infos[i] == nil {
				return ErrCheckpointChanged
			}
			found = true
		}
		if !found {
			return ErrCheckpointChanged
		}
	}
	return nil
}

// Writable import requires attachment-scoped durable mutation hooks. Historical
// completion callbacks and an arbitrary non-read-only backend do not grant it.
func (t *fileTorrentImpl) prepareRestore(ctx context.Context, claims FileCheckpoint, check func(context.Context) error) (*RestoreToken, error) {
	if (!t.client.opts.ReadOnly && t.resume.hooks == nil) || check == nil {
		return nil, ErrCheckpointUnsupported
	}
	if err := t.checkpointLock(ctx); err != nil {
		return nil, err
	}
	defer t.lifecycle.Unlock()
	if err := check(ctx); err != nil {
		return nil, err
	}
	current, infos, err := t.checkpointEvidence(ctx)
	if err != nil {
		return nil, err
	}
	if !sameCheckpointCatalog(claims, current) || !validCheckpointBits(claims.Complete, current.PieceCount) {
		return nil, ErrCheckpointChanged
	}
	current.Complete = bytes.Clone(claims.Complete)
	for i, f := range current.Files {
		if !f.Present || f.Size < f.Length || f != claims.Files[i] {
			if t.resume.hooks != nil {
				if err := t.revokeResumeFile(i); err != nil {
					return nil, err
				}
			}
			clearCheckpointFile(current.Complete, f, current.PieceLength)
		}
	}
	if !checkpointImportBudget(current) {
		return nil, ErrCheckpointBudget
	}
	if err := t.checkpointContributions(current, infos); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &RestoreToken{owner: t, ctx: ctx, epoch: t.checkpointEpoch.Load(), check: check, proof: current, infos: infos}, nil
}

func (t *fileTorrentImpl) restoreCheckpoint(ctx context.Context, token *RestoreToken) error {
	if token == nil || token.owner != t || token.used.Swap(true) {
		return ErrCheckpointChanged
	}
	if err := token.ctx.Err(); err != nil {
		return err
	}
	if err := t.checkpointLock(ctx); err != nil {
		return err
	}
	defer t.lifecycle.Unlock()
	if t.checkpointEpoch.Load() != token.epoch {
		return ErrCheckpointChanged
	}
	if err := token.check(ctx); err != nil {
		return err
	}
	current, _, err := t.checkpointEvidence(ctx)
	if err != nil {
		return err
	}
	if !sameCheckpointCatalog(token.proof, current) {
		return ErrCheckpointChanged
	}
	for i := range current.Files {
		if current.Files[i] != token.proof.Files[i] {
			return ErrCheckpointChanged
		}
	}
	if err := token.ctx.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.checkpointContributions(token.proof, token.infos); err != nil {
		return err
	}
	// Rooted opens above may block. Revalidate durable owner/generation after
	// that I/O, not only on entry. The adapter must retain its attachment /
	// mutation ownership through this callback and the following atomic import.
	if err := token.check(ctx); err != nil {
		return err
	}
	if err := token.ctx.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	t.rememberResume(token.proof.Complete, false)
	t.freshMu.Lock()
	defer t.freshMu.Unlock()
	if t.checkpointEpoch.Load() != token.epoch {
		return ErrCheckpointChanged
	}
	for i, f := range current.Files {
		if f.Length == 0 {
			t.files[i].zeroReady = f.Present
		}
	}
	if t.signatures == nil {
		t.signatures = make(map[int]*verifiedFileSignature)
	}
	for i, info := range token.infos {
		if info == nil {
			continue
		}
		old := t.signatures[i]
		if old == nil || !old.valid || !sameSignature(old.FileInfo, info) {
			if old != nil {
				old.valid = false
			}
			t.signatures[i] = &verifiedFileSignature{FileInfo: info, file: i, valid: true}
		}
	}
	for piece := 0; piece < current.PieceCount; piece++ {
		if !checkpointBit(token.proof.Complete, piece) {
			continue
		}
		var signature []os.FileInfo
		off := int64(piece) * current.PieceLength
		for i := range t.segmentLocater.LocateIter(segments.Extent{Start: off, Length: min(current.PieceLength, current.ExtentLength-off)}) {
			sig := t.signatures[i]
			signature = append(signature, sig)
		}
		t.markFreshLocked(piece, signature, true)
	}
	t.checkpointSealed.Store(t.checkpointEpoch.Add(1))
	return nil
}
