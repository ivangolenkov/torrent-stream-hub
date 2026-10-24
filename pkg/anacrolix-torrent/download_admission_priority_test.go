package torrent

import (
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

func TestDownloadAdmissionRefreshesPendingPieces(t *testing.T) {
	cl := newTestingClient(t)
	tor := cl.newTorrentForTesting()
	if err := tor.setInfoUnlocked(&metainfo.Info{Name: "data", Length: 16, PieceLength: 4, Pieces: make([]byte, 80)}); err != nil {
		t.Fatal(err)
	}
	// Model the state after an initial hash pass found four incomplete pieces.
	cl.lock()
	for i := range tor.pieces {
		tor.pieces[i].storageCompletionOk = true
	}
	cl.unlock()
	pending := func() uint64 {
		cl.rLock()
		defer cl.rUnlock()
		return tor._pendingPieces.GetCardinality()
	}
	for i := 0; i < 3; i++ {
		tor.Files()[0].SetPriority(PiecePriorityNormal)
		if got := pending(); got != 4 {
			t.Fatalf("enabled torrent has %d pending pieces", got)
		}
		tor.DisallowDataDownload()
		if got := pending(); got != 0 {
			t.Fatalf("disabled torrent retained %d pending pieces", got)
		}
		// Recheck/Pause change priorities after closing admission. Callbacks
		// must see consistent request-order and pending-piece indexes throughout.
		tor.Files()[0].SetPriority(PiecePriorityNone)
		tor.AllowDataDownload()
		if got := pending(); got != 0 {
			t.Fatalf("enabling an unselected file requested %d pieces", got)
		}
	}
}
