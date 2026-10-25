package torrent

import (
	"github.com/anacrolix/missinggo/v2/bitmap"
	"testing"
)

func TestFileObservationsSharedPieceAndFailedCheck(t *testing.T) {
	cl, tor, _ := createTestMultiFileTorrent(t, 4, []int64{3, 3, 0})
	cl.lock()
	// No storage access is possible. The projection must use cached facts.
	tor.storage = nil
	tor.piecesQueuedForHash.Clear()
	for i := range tor.pieces {
		tor.pieces[i].numVerifies = 1
		tor.pieces[i].storageCompletionOk = true
	}
	tor._completedPieces.Add(bitmap.BitIndex(0))
	cl.unlock()
	files := tor.FileObservations()
	if files[0].VerifiedBytes != 3 || files[1].VerifiedBytes != 1 || files[1].CheckedBytes != 3 || files[1].DirectReadReady {
		t.Fatalf("boundary/readiness %+v", files)
	}
	if !files[0].DirectReadReady {
		t.Fatal("checked shared piece not ready")
	}
	cl.lock()
	tor.piecesQueuedForHash.Add(0)
	cl.unlock()
	files = tor.FileObservations()
	if files[0].VerifiedBytes != 0 || files[0].CheckedBytes != 0 || files[0].QueuedPieces != 1 || files[1].CheckedBytes != 2 {
		t.Fatalf("queued verification counted as completed: %+v", files)
	}
}
