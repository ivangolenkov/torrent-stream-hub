package torrent

import (
	"strings"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

func TestV2MissingHashesDoNotAdmitPayloadRequests(t *testing.T) {
	cl := newTestingClient(t)
	tor := cl.newTorrentForTesting()
	tor.DisallowDataDownload()
	info := &metainfo.Info{Name: "v2", MetaVersion: 2, PieceLength: 16384, FileTree: metainfo.FileTree{Dir: map[string]metainfo.FileTree{"file": {File: metainfo.FileTreeFile{Length: 32768, PiecesRoot: strings.Repeat("x", 32)}}}}}
	if err := tor.setInfoUnlocked(info); err != nil {
		t.Fatal(err)
	}
	tor.DownloadAll()
	cl.lock()
	if !tor._pendingPieces.IsEmpty() || tor.needData() {
		t.Fatal("disabled torrent requested hash or payload data")
	}
	cl.unlock()
	tor.AllowDataDownload()
	cl.lock()
	if !tor._pendingPieces.IsEmpty() {
		t.Fatal("unknown hashes admitted payload requests")
	}
	if !tor.needData() {
		t.Fatal("enabled selected files lost hash discovery")
	}
	cl.unlock()
}

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
