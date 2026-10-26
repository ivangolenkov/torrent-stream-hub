package torrent

import (
	"context"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

var fileCheckpointSlot = make(chan struct{}, 1)

// FileResumeObservation returns immutable facts from this backend open only.
func (t *Torrent) FileResumeObservation() storage.ResumeObservation {
	t.cl.rLock()
	defer t.cl.rUnlock()
	if t.closed.IsSet() || t.storage == nil || t.storage.ResumeObservation == nil {
		return storage.ResumeObservation{}
	}
	return t.storage.ResumeObservation()
}

// FileCheckpointMetainfo snapshots only metadata needed for resume binding, not
// trackers/webseeds. Bound layer materialization BEFORE pieceLayers allocates.
func (t *Torrent) FileCheckpointMetainfo() (metainfo.MetaInfo, error) {
	t.cl.rLock()
	defer t.cl.rUnlock()
	if t.closed.IsSet() || t.info == nil || t.files == nil {
		return metainfo.MetaInfo{}, storage.ErrCheckpointUnsupported
	}
	if len(*t.files) > 4096 {
		return metainfo.MetaInfo{}, storage.ErrCheckpointBudget
	}
	var layers int64
	for _, f := range *t.files {
		if f.piecesRoot.Ok {
			layers += int64(f.EndPieceIndex()-f.BeginPieceIndex()) * 32
		}
	}
	metadata := int64(len(t.metadataBytes)) + layers
	pieces := int64(t.info.NumPieces())
	if metadata > 4<<20 || len(*t.files) > 4096 || pieces > 4<<20 || pieces*256+int64(len(*t.files))*2048+((pieces+7)/8)*8+metadata*4 > 32<<20 {
		return metainfo.MetaInfo{}, storage.ErrCheckpointBudget
	}
	return metainfo.MetaInfo{InfoBytes: t.metadataBytes, PieceLayers: t.pieceLayers()}, nil
}

// FileCheckpointState samples cached backend revisions only. It does not stat,
// hash, flush or authorize a seal, and cannot grant payload readiness.
func (t *Torrent) FileCheckpointState() (revision uint64, dirty bool) {
	t.cl.rLock()
	defer t.cl.rUnlock()
	if t.closed.IsSet() || t.storage == nil || t.storage.CheckpointState == nil {
		return 0, false
	}
	return t.storage.CheckpointState()
}

// waitCheckpointLocked applies bounded network backpressure to this torrent,
// releasing Client.lock while waiting. The caller must revalidate state afterwards.
func (t *Torrent) waitCheckpointLocked(peerClosed <-chan struct{}) error {
	for t.checkpointDone != nil {
		done := t.checkpointDone
		t.cl.unlock()
		select {
		case <-done:
		case <-t.closed.Done():
		case <-peerClosed:
		}
		t.cl.lock()
		if t.closed.IsSet() {
			return errTorrentClosed
		}
		select {
		case <-peerClosed:
			return errTorrentClosed
		default:
		}
	}
	return t.getClosedErr()
}

// WithFileCheckpoint temporarily closes hash/layer/write admission for this
// torrent, joins admitted operations, then flushes/publishes outside Client.lock.
// It never Drops the torrent or closes readers. The callback must not reenter the
// library; it normally commits a generation-fenced SQLite snapshot. Runtime use
// additionally requires application ownership and durable mutation hooks.
func (t *Torrent) WithFileCheckpoint(ctx context.Context, publish func(storage.FileCheckpoint) error) error {
	job, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(t.closedCtx, cancel)
	defer func() { stop(); cancel() }()
	select {
	case fileCheckpointSlot <- struct{}{}:
		defer func() { <-fileCheckpointSlot }()
	case <-job.Done():
		return job.Err()
	}
	t.cl.lock()
	backend := t.storage
	if t.closed.IsSet() {
		t.cl.unlock()
		return errTorrentClosed
	}
	if backend == nil || backend.WithCheckpoint == nil || t.info == nil || t.storageOpening != nil || t.checkpointDone != nil {
		t.cl.unlock()
		return storage.ErrCheckpointUnsupported
	}
	// Check budgets before allocating the eligibility mask.
	count := t.info.NumPieces()
	if count > 4<<20 || len(*t.files) > 4096 {
		t.cl.unlock()
		return storage.ErrCheckpointBudget
	}
	done := make(chan struct{})
	t.checkpointDone = done
	t.cl.storageTasks.Add(1)
	t.cl.unlock()
	defer func() {
		t.cl.lock()
		t.checkpointDone = nil
		close(done)
		if !t.closed.IsSet() {
			t.cl.startPieceHashers()
		}
		t.cl.unlock()
		t.cl.storageTasks.Done()
	}()
	// This deadline bounds only admission/join waiting, not an issued fsync.
	admission, stopAdmission := context.WithTimeout(job, 2*time.Second)
	defer stopAdmission()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		t.cl.lock()
		closed := t.closed.IsSet()
		idle := t.activePieceHashes == 0 && t.hashLayerUpdates == 0 && t.activeCheckpointWrites == 0
		if idle && !closed && admission.Err() == nil {
			eligible := make([]byte, (count+7)/8)
			for i := range t.pieces {
				p := &t.pieces[i]
				if t.pieceComplete(i) && p.storageCompletionOk && p.haveHash() && !p.marking && !p.hashing && !p.queuedForHash() && !p.hasPendingWrites() && !p.hasDirtyChunks() {
					eligible[i/8] |= 1 << uint(i%8)
				}
			}
			t.cl.unlock()
			err := backend.WithCheckpoint(job, eligible, publish)
			if err == nil {
				t.cl.lock()
				err = t.getClosedErr()
				t.cl.unlock()
			}
			return err
		}
		t.cl.unlock()
		if closed {
			return errTorrentClosed
		}
		select {
		case <-admission.Done():
			return admission.Err()
		case <-tick.C:
		}
	}
}
