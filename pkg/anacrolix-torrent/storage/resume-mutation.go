package storage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/segments"
)

var ErrUnverifiedPayload = errors.New("payload is not verified for this attachment")

// FileResumeHooks is scoped to ONE backend open. Callbacks must durably fence the
// exact record/mapping/attachment and may do SQL, but must not reenter the library
// or acquire application command reservations. Errors forbid the mutation.
type FileResumeHooks struct {
	Observation      ResumeObservation // populated before backend publication; immutable afterwards
	Pending          []byte            // persisted, not-yet-revoked pieces, never readiness by itself
	RestoreCandidate bool              // do not create missing empty files merely to restore
	Revoke           func([]byte) (discarded bool, err error)
	Invalidate       func() error
	// Restore runs once, before this open is published to the library. The
	// caller's open context is attachment-scoped and Drop/Close joins the call.
	// An error must not leave imported readiness behind; the open is discarded.
	Restore func(context.Context, TorrentImpl) error
}

// ResumeObservation describes this open's restore attempt, not readiness authority.
type ResumeObservation struct {
	Mode, Status, Reason string
	Generation           string
	CheckpointAtMS       int64
}

func (t *fileTorrentImpl) refreshEmptyFile(index int) {
	if index < 0 || index >= len(t.files) || t.metainfoFileInfos[index].Length != 0 || t.readClosed.Load() {
		return
	}
	t.files[index].payloadMu.RLock()
	defer t.files[index].payloadMu.RUnlock()
	if t.readClosed.Load() {
		return
	}
	_, err := t.stat(t.files[index].safeOsPath)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	t.freshMu.Lock()
	if t.files[index].zeroReady != (err == nil) {
		t.checkpointEpoch.Add(1)
	}
	t.files[index].zeroReady = err == nil
	t.freshMu.Unlock()
}

func (t *fileTorrentImpl) cachedEmptyFile(index int) bool {
	if t.readClosed.Load() {
		return false
	}
	t.freshMu.Lock()
	defer t.freshMu.Unlock()
	return index >= 0 && index < len(t.files) && t.files[index].zeroReady
}

// Existing file order is torrent-offset order. Lease a contiguous index span
// without per-chunk allocations; zero-length files in that span are harmless.
// These are syscall read leases, not a second application command registry.
func (t *fileTorrentImpl) lockPayloadRange(off, length int64, write bool) func() {
	first, last := -1, -1
	for i := range t.segmentLocater.LocateIter(segments.Extent{Start: off, Length: length}) {
		if first < 0 {
			first = i
		}
		last = i
	}
	if first < 0 {
		return func() {}
	}
	for i := first; i <= last; i++ {
		if write {
			if !t.files[i].payloadMu.TryLock() {
				start := time.Now()
				t.ioTimings.writeWaitActive.Add(1)
				t.files[i].payloadMu.Lock()
				ns := time.Since(start).Nanoseconds()
				t.ioTimings.writeWaitNS.Add(ns)
				recordIOMax(&t.ioTimings.writeWaitMaxNS, ns)
				t.ioTimings.writeWaitCount.Add(1)
				t.ioTimings.writeWaitActive.Add(-1)
			}
		} else {
			t.files[i].payloadMu.RLock()
		}
	}
	return func() {
		for i := last; i >= first; i-- {
			if write {
				t.files[i].payloadMu.Unlock()
			} else {
				t.files[i].payloadMu.RUnlock()
			}
		}
	}
}

type FileResumeFactory func(context.Context, *metainfo.Info, metainfo.Hash) (*FileResumeHooks, error)

type resumeMutationState struct {
	mu      sync.Mutex // metadata/revocation coalescing; lifecycle is the mutation fence
	hooks   *FileResumeHooks
	pending []byte
}

func (t *fileTorrentImpl) revokeResume(begin, end int) error {
	t.resume.mu.Lock()
	defer t.resume.mu.Unlock()
	if t.resume.hooks == nil || len(t.resume.pending) == 0 {
		return nil
	}
	changed := false
	for i := begin; i < end; i++ {
		if checkpointBit(t.resume.pending, i) {
			changed = true
			break
		}
	}
	if !changed {
		return nil
	}
	mask := make([]byte, len(t.resume.pending))
	for i := begin; i < end; i++ {
		if checkpointBit(t.resume.pending, i) {
			mask[i/8] |= 1 << uint(i%8)
		}
	}
	discarded, err := t.resume.hooks.Revoke(mask)
	if err != nil {
		return err
	}
	if discarded {
		clear(t.resume.pending)
	} else {
		for i := range mask {
			t.resume.pending[i] &^= mask[i]
		}
	}
	t.freshMu.Lock()
	for i := begin; i < end; i++ {
		if checkpointBit(mask, i) {
			t.forgetPieceLocked(i)
			delete(t.hashSnapshot, i)
		}
	}
	t.checkpointEpoch.Add(1)
	t.freshMu.Unlock()
	return nil
}

func (t *fileTorrentImpl) rememberResume(mask []byte, replace bool) {
	t.resume.mu.Lock()
	defer t.resume.mu.Unlock()
	if t.resume.hooks == nil {
		return
	}
	if replace || len(t.resume.pending) == 0 {
		t.resume.pending = bytes.Clone(mask)
		return
	}
	for i := range mask {
		t.resume.pending[i] |= mask[i]
	}
}

func (t *fileTorrentImpl) revokeResumeFile(index int) error {
	f := t.metainfoFileInfos[index]
	if f.Length == 0 {
		return nil
	}
	t.freshMu.Lock()
	t.checkpointEpoch.Add(1)
	if sig := t.signatures[index]; sig != nil {
		sig.valid = false
		sig.complete = 0
		sig.published = false
	}
	t.freshMu.Unlock()
	return t.revokeResume(int(f.TorrentOffset/t.info.PieceLength), int((f.TorrentOffset+f.Length-1)/t.info.PieceLength)+1)
}

// Creating a missing file or extending a truncated file can synthesize zeros
// outside this chunk. Revoke the entire contributing file BEFORE O_CREATE or
// extension, not merely the requested piece. No SQL/stat is needed when that
// file has no remaining persisted pieces.
func (t *fileTorrentImpl) guardResumeFileWrite(index int) error {
	f := t.metainfoFileInfos[index]
	if f.Length == 0 {
		return nil
	}
	t.resume.mu.Lock()
	wanted := false
	if t.resume.hooks != nil && len(t.resume.pending) != 0 {
		for i, end := int(f.TorrentOffset/t.info.PieceLength), int((f.TorrentOffset+f.Length-1)/t.info.PieceLength)+1; i < end; i++ {
			if checkpointBit(t.resume.pending, i) {
				wanted = true
				break
			}
		}
	}
	t.resume.mu.Unlock()
	if !wanted {
		return nil
	}
	st, err := t.stat(t.files[index].safeOsPath)
	t.freshMu.Lock()
	sig := t.signatures[index]
	unchanged := sig != nil && sig.valid && sameSignature(sig.FileInfo, st)
	t.freshMu.Unlock()
	if err == nil && st.Size() >= f.Length && unchanged {
		return nil
	}
	revokeErr := t.revokeResumeFile(index)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(err, revokeErr)
	}
	return revokeErr
}

func (t *fileTorrentImpl) revokeCheckpoint(begin, end int) error {
	if begin < 0 || end < begin || end > t.info.NumPieces() {
		return ErrCheckpointChanged
	}
	if !t.beginOperation() {
		return ErrCheckpointChanged
	}
	defer t.lifecycle.RUnlock()
	t.checkpointEpoch.Add(1)
	// A known-invalid layer/hash must stop live reads even if SQLite failed.
	t.freshMu.Lock()
	for i := begin; i < end; i++ {
		t.forgetPieceLocked(i)
		delete(t.hashSnapshot, i)
	}
	t.freshMu.Unlock()
	return t.revokeResume(begin, end)
}
