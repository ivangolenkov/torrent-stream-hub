package torrent

// FileObservation samples the current open only. It performs no storage I/O.
// CheckedBytes includes failed hash attempts; VerifiedBytes does not. A direct
// open must refresh storage evidence even when DirectReadReady was observed.
type FileObservation struct {
	// Bytes excluded from the startup hash plan as missing/short, and not yet
	// attempted since. Includes present portions of an incomplete boundary piece;
	// historical planning evidence, never current file presence.
	SkippedMissingBytes                 int64
	RestoredBytes, FreshlyVerifiedBytes int64
	ReaderPieces                        int
	VerifiedBytes, CheckedBytes         int64
	QueuedPieces, ActivePieces          int
	EffectivePriority                   PiecePriority
	DirectReadReady                     bool
}

func (t *Torrent) FileObservations() []FileObservation {
	t.cl.rLock()
	defer t.cl.rUnlock()
	if t.closed.IsSet() || t.files == nil || t.info == nil {
		return nil
	}
	out := make([]FileObservation, len(*t.files))
	for i, f := range *t.files {
		o := &out[i]
		o.DirectReadReady = f.cachedReadyForDirectReadLocked()
		if f.length == 0 {
			continue
		}
		for index := f.BeginPieceIndex(); index < f.EndPieceIndex(); index++ {
			p := &t.pieces[index]
			if t.readerNowPieces().Contains(uint32(index)) {
				o.ReaderPieces++
			}
			start := max(f.offset, int64(index)*t.info.PieceLength)
			end := min(f.offset+f.length, int64(index+1)*t.info.PieceLength)
			n := max(int64(0), end-start)
			queued := p.queuedForHash()
			busy := queued || p.hashing || p.marking
			if queued || p.hashWaiting.Load() {
				o.QueuedPieces++
			}
			if p.hashing && !p.hashWaiting.Load() || p.marking {
				o.ActivePieces++
			}
			if p.numVerifies > 0 && !busy {
				o.CheckedBytes += n
			} else if p.initialVerificationSkipped && p.numVerifies == 0 && !busy {
				o.SkippedMissingBytes += n
			}
			if t.pieceComplete(index) && p.storageCompletionOk && !busy && !p.hasDirtyChunks() && !p.hasPendingWrites() {
				o.VerifiedBytes += n
				if t.storage != nil && t.storage.CachedRestored != nil && t.storage.CachedRestored(int(index)) {
					o.RestoredBytes += n
				} else if p.numVerifies > 0 && (t.storage == nil || t.storage.CachedComplete == nil || t.storage.CachedComplete(int(index))) {
					o.FreshlyVerifiedBytes += n
				}
			}
			o.EffectivePriority = max(o.EffectivePriority, p.effectivePriority())
		}
	}
	return out
}
