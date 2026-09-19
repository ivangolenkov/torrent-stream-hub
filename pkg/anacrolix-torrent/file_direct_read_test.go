package torrent

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/missinggo/v2/bitmap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/anacrolix/torrent/metainfo"
)

func createTestMultiFileTorrent(t testing.TB, pieceLength int64, fileLengths []int64) (*Client, *Torrent, []*File) {
	cl := newTestingClient(t)
	tor := cl.newTorrentForTesting()

	var totalLen int64
	var files []metainfo.FileInfo
	for i, l := range fileLengths {
		files = append(files, metainfo.FileInfo{
			Length: l,
			Path:   []string{"dir", "file" + string(rune('A'+i))},
		})
		totalLen += l
	}

	numPieces := int((totalLen + pieceLength - 1) / pieceLength)
	if numPieces == 0 {
		numPieces = 1
	}

	info := &metainfo.Info{
		PieceLength: pieceLength,
		Pieces:      make([]byte, metainfo.HashSize*numPieces),
		Files:       files,
		Length:      0, // multi-file torrent
	}

	require.NoError(t, tor.setInfoUnlocked(info))
	return cl, tor, tor.Files()
}

func markPieceComplete(tor *Torrent, pieceIdx int) {
	tor.cl.lock()
	defer tor.cl.unlock()
	tor._completedPieces.Add(bitmap.BitIndex(pieceIdx))
	tor.pieces[pieceIdx].storageCompletionOk = true
}

func TestFileReadyForDirectRead_NilAndClosed(t *testing.T) {
	var nilFile *File
	assert.False(t, nilFile.ReadyForDirectRead())

	cl := newTestingClient(t)
	tor := cl.newTorrentForTesting()

	// File before metadata
	fileBeforeMeta := &File{t: tor, length: 100}
	assert.False(t, fileBeforeMeta.ReadyForDirectRead())

	// Closed torrent
	_, tor2, files := createTestMultiFileTorrent(t, 1024, []int64{2048})
	tor2.Drop()
	assert.False(t, files[0].ReadyForDirectRead())
}

func TestFileReadyForDirectRead_ZeroLength(t *testing.T) {
	_, _, files := createTestMultiFileTorrent(t, 1024, []int64{0, 2048})
	// Zero-length file should report ready without checking pieces
	assert.True(t, files[0].ReadyForDirectRead())
	// Non-completed second file should report false
	assert.False(t, files[1].ReadyForDirectRead())
}

func TestFileReadyForDirectRead_V2AlignedFiles(t *testing.T) {
	cl := newTestingClient(t)
	tor := cl.newTorrentForTesting()
	info := &metainfo.Info{
		MetaVersion: 2,
		PieceLength: 16384,
		Name:        "aligned",
		FileTree: metainfo.FileTree{Dir: map[string]metainfo.FileTree{
			"a": {File: metainfo.FileTreeFile{Length: 1000, PiecesRoot: strings.Repeat("a", 32)}},
			"b": {File: metainfo.FileTreeFile{Length: 1000, PiecesRoot: strings.Repeat("b", 32)}},
			"c": {File: metainfo.FileTreeFile{Length: 0}},
		}},
	}
	require.NoError(t, tor.setInfoUnlocked(info))
	files := tor.Files()
	require.EqualValues(t, 16384, files[1].Offset())
	require.Greater(t, files[1].Offset(), info.TotalLength())
	markPieceComplete(tor, 0)
	assert.True(t, files[0].ReadyForDirectRead())
	assert.False(t, files[1].ReadyForDirectRead(), "alignment must not bypass piece completion")
	markPieceComplete(tor, 1)
	assert.True(t, files[1].ReadyForDirectRead())
	assert.True(t, files[2].ReadyForDirectRead(), "empty trailing file also has an aligned offset")

	tor.cl.lock()
	tor.pieces[1].hashing = true
	tor.cl.unlock()
	assert.False(t, files[1].ReadyForDirectRead(), "rechecking still blocks direct reads")
	tor.cl.lock()
	tor.pieces[1].hashing = false
	tor.cl.unlock()
}

func TestFileReadyForDirectRead_CompleteAndIncomplete(t *testing.T) {
	_, tor, files := createTestMultiFileTorrent(t, 1024, []int64{2048}) // 2 pieces
	f := files[0]

	// 0 pieces complete -> false
	assert.False(t, f.ReadyForDirectRead())

	// 1 piece complete -> false
	markPieceComplete(tor, 0)
	assert.False(t, f.ReadyForDirectRead())

	// All pieces complete -> true
	markPieceComplete(tor, 1)
	assert.True(t, f.ReadyForDirectRead())

	// Storage completion not ok -> false
	tor.cl.lock()
	tor.pieces[0].storageCompletionOk = false
	tor.cl.unlock()
	assert.False(t, f.ReadyForDirectRead())
	tor.cl.lock()
	tor.pieces[0].storageCompletionOk = true
	tor.cl.unlock()
	assert.True(t, f.ReadyForDirectRead())

	// Hashing -> false
	tor.cl.lock()
	tor.pieces[1].hashing = true
	tor.cl.unlock()
	assert.False(t, f.ReadyForDirectRead())
	tor.cl.lock()
	tor.pieces[1].hashing = false
	tor.cl.unlock()
	assert.True(t, f.ReadyForDirectRead())

	// QueuedForHash -> false
	tor.cl.lock()
	tor.piecesQueuedForHash.Add(pieceIndex(0))
	tor.cl.unlock()
	assert.False(t, f.ReadyForDirectRead())
	tor.cl.lock()
	tor.piecesQueuedForHash.Remove(pieceIndex(0))
	tor.cl.unlock()
	assert.True(t, f.ReadyForDirectRead())

	// Marking -> false
	tor.cl.lock()
	tor.pieces[1].marking = true
	tor.cl.unlock()
	assert.False(t, f.ReadyForDirectRead())
	tor.cl.lock()
	tor.pieces[1].marking = false
	tor.cl.unlock()
	assert.True(t, f.ReadyForDirectRead())

	// Pending writes -> false
	tor.pieces[0].incrementPendingWrites()
	assert.False(t, f.ReadyForDirectRead())
	tor.pieces[0].decrementPendingWrites()
	assert.True(t, f.ReadyForDirectRead())

	// Dirty chunks -> false
	tor.cl.lock()
	tor.dirtyChunks.Add(tor.pieces[0].requestIndexBegin())
	tor.cl.unlock()
	assert.False(t, f.ReadyForDirectRead())
	tor.cl.lock()
	tor.dirtyChunks.Remove(tor.pieces[0].requestIndexBegin())
	tor.cl.unlock()
	assert.True(t, f.ReadyForDirectRead())
}

func TestFileReadyForDirectRead_SharedBoundaryPiece(t *testing.T) {
	// PieceLength = 1000.
	// FileA: length = 1500 (pieces 0 and 1)
	// FileB: length = 1500 (pieces 1 and 2)
	// Piece 1 is shared by FileA and FileB.
	_, tor, files := createTestMultiFileTorrent(t, 1000, []int64{1500, 1500})
	fileA := files[0]
	fileB := files[1]

	markPieceComplete(tor, 0)
	markPieceComplete(tor, 1)

	// Pieces 0 and 1 are complete.
	// FileA requires pieces 0, 1 -> Ready!
	// FileB requires pieces 1, 2 -> Not ready (piece 2 incomplete).
	assert.True(t, fileA.ReadyForDirectRead())
	assert.False(t, fileB.ReadyForDirectRead())

	// If shared piece 1 becomes hashing, FileA is no longer ready
	tor.cl.lock()
	tor.pieces[1].hashing = true
	tor.cl.unlock()
	assert.False(t, fileA.ReadyForDirectRead())
	assert.False(t, fileB.ReadyForDirectRead())

	tor.cl.lock()
	tor.pieces[1].hashing = false
	tor.cl.unlock()

	// Once piece 2 is also complete, FileB becomes ready too
	markPieceComplete(tor, 2)
	assert.True(t, fileA.ReadyForDirectRead())
	assert.True(t, fileB.ReadyForDirectRead())
}

func TestFileReadyForDirectRead_NoSideEffects(t *testing.T) {
	_, tor, files := createTestMultiFileTorrent(t, 1024, []int64{2048})
	f := files[0]
	markPieceComplete(tor, 0)
	markPieceComplete(tor, 1)

	tor.cl.lock()
	prioBefore := tor.pieces[0].priority
	tor.cl.unlock()

	ready := f.ReadyForDirectRead()
	assert.True(t, ready)

	// No readers created
	tor.cl.lock()
	assert.Empty(t, tor.readers)
	assert.Equal(t, prioBefore, tor.pieces[0].priority)
	tor.cl.unlock()

	allocs := testing.AllocsPerRun(100, func() {
		f.ReadyForDirectRead()
	})
	assert.LessOrEqual(t, allocs, float64(4), "expected at most 4 allocs from anacrolix sync mutex wrapper")
}

func BenchmarkFileReadyForDirectRead(b *testing.B) {
	const (
		numPieces   = 10000
		pieceLength = 256 << 10
	)
	cl := newTestingClient(b)
	tor := cl.newTorrentForTesting()

	info := &metainfo.Info{
		PieceLength: pieceLength,
		Pieces:      make([]byte, metainfo.HashSize*numPieces),
		Files: []metainfo.FileInfo{
			{Length: pieceLength * numPieces, Path: []string{"dir", "bigfile.mkv"}},
		},
	}
	require.NoError(b, tor.setInfoUnlocked(info))
	f := tor.Files()[0]

	for i := 0; i < numPieces; i++ {
		tor._completedPieces.Add(bitmap.BitIndex(i))
		tor.pieces[i].storageCompletionOk = true
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !f.ReadyForDirectRead() {
			b.Fatal("expected ready")
		}
	}
}

func TestDirectRead_IndependentFromClientMutex(t *testing.T) {
	cl := newTestingClient(t)
	tor := cl.newTorrentForTesting()
	dir := t.TempDir()

	info := &metainfo.Info{
		PieceLength: 1024,
		Pieces:      make([]byte, metainfo.HashSize*2),
		Files: []metainfo.FileInfo{
			{Length: 2048, Path: []string{"video.mp4"}},
		},
	}
	require.NoError(t, tor.setInfoUnlocked(info))
	f := tor.Files()[0]

	for i := 0; i < 2; i++ {
		tor._completedPieces.Add(bitmap.BitIndex(i))
		tor.pieces[i].storageCompletionOk = true
	}

	require.True(t, f.ReadyForDirectRead())

	// Create and write the actual file
	filePath := filepath.Join(dir, "video.mp4")
	data := bytes.Repeat([]byte("M"), 2048)
	require.NoError(t, os.WriteFile(filePath, data, 0o644))

	osFile, err := os.Open(filePath)
	require.NoError(t, err)
	defer osFile.Close()

	// Acquire client mutex in a separate goroutine and hold it
	lockAcquired := make(chan struct{})
	releaseLock := make(chan struct{})
	go func() {
		cl.lock()
		close(lockAcquired)
		<-releaseLock
		cl.unlock()
	}()

	<-lockAcquired

	// Read and seek while client mutex is locked
	readDone := make(chan struct{})
	go func() {
		buf := make([]byte, 512)
		n, err := osFile.Read(buf)
		require.NoError(t, err)
		require.Equal(t, 512, n)
		pos, err := osFile.Seek(1024, io.SeekStart)
		require.NoError(t, err)
		require.Equal(t, int64(1024), pos)
		close(readDone)
	}()

	select {
	case <-readDone:
		// Succeeded while client lock was held!
	case <-time.After(1 * time.Second):
		t.Fatal("direct read blocked by client mutex")
	}

	close(releaseLock)
}
