package torrent

import (
	"github.com/RoaringBitmap/roaring"
	g "github.com/anacrolix/generics"
)

// SetVerificationSelection keeps durable selection separate from network
// priorities and streaming overlays, including while the torrent is paused.
func (t *Torrent) SetVerificationSelection(indices []int) {
	t.cl.lock()
	defer t.cl.unlock()
	if t.files == nil || t.closed.IsSet() {
		return
	}
	t.verificationSelected.Clear()
	for _, index := range indices {
		if index < 0 || index >= len(*t.files) {
			continue
		}
		f := (*t.files)[index]
		if f.length > 0 {
			t.verificationSelected.AddRange(uint64(f.BeginPieceIndex()), uint64(f.EndPieceIndex()))
		}
	}
	t.verificationSelectionSet = true
}

func (t *Torrent) verificationPriority(index pieceIndex) int {
	if t.readerNowPieces().Contains(uint32(index)) {
		return 3
	}
	if t.readerReadaheadPieces().Contains(uint32(index)) {
		return 2
	}
	if t.receivedPiecesForHash.Contains(uint32(index)) {
		return 2
	}
	if !t.cl.config.VerificationBackground && (!t.verificationSelectionSet || t.verificationSelected.Contains(uint32(index))) {
		return 1
	}
	return 0
}

func (t *Torrent) firstQueuedIn(mask *roaring.Bitmap) (_ g.Option[pieceIndex]) {
	if mask == nil || mask.IsEmpty() {
		return
	}
	queued, wanted := t.piecesQueuedForHash.Bitmap.Iterator(), mask.Iterator()
	for queued.HasNext() && wanted.HasNext() {
		q, w := queued.PeekNext(), wanted.PeekNext()
		if q < w {
			queued.AdvanceIfNeeded(w)
			continue
		}
		if w < q {
			wanted.AdvanceIfNeeded(q)
			continue
		}
		p := t.piece(int(q))
		if !p.hashing && !p.marking {
			return g.Some(int(q))
		}
		queued.Next()
		wanted.Next()
	}
	return
}

func (t *Torrent) hashCandidate(fair bool) (_ g.Option[pieceIndex]) {
	if !fair {
		if p := t.firstQueuedIn(t.readerNowPieces().RB); p.Ok {
			return p
		}
		if p := t.firstQueuedIn(t.readerReadaheadPieces().RB); p.Ok {
			return p
		}
		if p := t.firstQueuedIn(&t.receivedPiecesForHash); p.Ok {
			return p
		}
		if !t.cl.config.VerificationBackground && t.verificationSelectionSet {
			if p := t.firstQueuedIn(&t.verificationSelected); p.Ok {
				return p
			}
		}
	}
	it := t.piecesQueuedForHash.Bitmap.Iterator()
	if fair && t.hashBackgroundCursor > 0 {
		it.AdvanceIfNeeded(uint32(t.hashBackgroundCursor))
	}
	for it.HasNext() {
		i := int(it.Next())
		p := t.piece(i)
		if !p.hashing && !p.marking {
			return g.Some(i)
		}
	}
	if fair && t.hashBackgroundCursor > 0 {
		for i := range t.piecesQueuedForHash.Iterate {
			p := t.piece(i)
			if !p.hashing && !p.marking {
				return g.Some(i)
			}
		}
	}
	return
}
