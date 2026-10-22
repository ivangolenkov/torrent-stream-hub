package engine

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/models"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

func createTestTorrentBytes(pieceLength int64, name string, files []metainfo.FileInfo, fileData [][]byte) []byte {
	var allData []byte
	for _, d := range fileData {
		allData = append(allData, d...)
	}
	numPieces := int((int64(len(allData)) + pieceLength - 1) / pieceLength)
	if numPieces == 0 {
		numPieces = 1
	}

	pieces := make([]byte, 0, numPieces*20)
	for i := 0; i < numPieces; i++ {
		start := int64(i) * pieceLength
		end := start + pieceLength
		if end > int64(len(allData)) {
			end = int64(len(allData))
		}
		var chunk []byte
		if start < int64(len(allData)) {
			chunk = allData[start:end]
		}
		h := sha1.Sum(chunk)
		pieces = append(pieces, h[:]...)
	}

	info := metainfo.Info{
		Name:        name,
		PieceLength: pieceLength,
		Pieces:      pieces,
		Files:       files,
	}
	if len(files) == 1 && len(files[0].Path) == 0 {
		info.Length = files[0].Length
		info.Files = nil
	}

	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		panic(err)
	}
	mi := metainfo.MetaInfo{
		InfoBytes: infoBytes,
	}
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func setupEngineWithTorrent(t *testing.T, pieceLength int64, name string, files []metainfo.FileInfo, fileData [][]byte) (*Engine, *models.Torrent, string) {
	t.Helper()
	dir := t.TempDir()

	cfg := &config.Config{
		DownloadDir:  dir,
		DBPath:       filepath.Join(dir, "hub.db"),
		TorrentPort:  0,
		BTDisableDHT: true,
		BTDisablePEX: true,
		BTNoUpload:   true,
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	t.Cleanup(func() {
		e.Close()
	})

	torrentBytes := createTestTorrentBytes(pieceLength, name, files, fileData)
	m, err := e.AddTorrentFile(bytes.NewReader(torrentBytes))
	if err != nil {
		t.Fatalf("failed to add torrent file: %v", err)
	}
	raw := e.GetRawTorrent(m.Hash)
	if err := raw.VerifyData(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		pending := false
		for i := 0; i < raw.NumPieces(); i++ {
			state := raw.Piece(i).State()
			pending = pending || state.Hashing || state.Checking || state.Marking
		}
		if !pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("initial missing-data verification did not finish")
		}
		time.Sleep(time.Millisecond)
	}

	return e, m, filepath.Join(dir, "payload", m.Hash)
}

func verifyEngineTorrentPieces(t *testing.T, e *Engine, hash string, begin, end int) {
	t.Helper()
	e.mu.RLock()
	mt := e.managedTorrents[hash]
	e.mu.RUnlock()

	for i := begin; i < end; i++ {
		if err := mt.t.Piece(i).VerifyData(); err != nil {
			t.Fatalf("failed to verify piece %d: %v", i, err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for i := begin; i < end; i++ {
		p := mt.t.Piece(i)
		for p.State().Marking && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestStreamSource_FallbackWhenNotReady(t *testing.T) {
	pieceLen := int64(1024)
	data := make([]byte, 2048)
	files := []metainfo.FileInfo{
		{Length: 2048, Path: []string{"movie.mp4"}},
	}
	e, tor, _ := setupEngineWithTorrent(t, pieceLen, "test_torrent", files, [][]byte{data})

	ctx := context.Background()
	source, err := e.OpenStreamSource(ctx, tor.Hash, 0, false)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	defer source.Close()

	if source.Type != StreamSourceTorrent {
		t.Fatalf("expected StreamSourceTorrent, got %s", source.Type)
	}
	if source.FallbackReason != FallbackReasonNotReady {
		t.Fatalf("expected FallbackReasonNotReady, got %s", source.FallbackReason)
	}
	if source.Length != 2048 {
		t.Fatalf("expected length 2048, got %d", source.Length)
	}
}

func TestStreamSource_DiskSuccess(t *testing.T) {
	pieceLen := int64(1024)
	data := make([]byte, 2048)
	for i := range data {
		data[i] = byte(i % 251)
	}
	files := []metainfo.FileInfo{
		{Length: 2048, Path: []string{"video.mkv"}},
	}
	e, tor, dir := setupEngineWithTorrent(t, pieceLen, "my_torrent", files, [][]byte{data})

	// Create completed file on disk matching layout
	targetDir := filepath.Join(dir, "my_torrent")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	targetFile := filepath.Join(targetDir, "video.mkv")
	if err := os.WriteFile(targetFile, data, 0o644); err != nil {
		t.Fatal(err)
	}

	// Verify pieces from disk
	verifyEngineTorrentPieces(t, e, tor.Hash, 0, 2)

	ctx := context.Background()
	source, err := e.OpenStreamSource(ctx, tor.Hash, 0, false)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	defer source.Close()

	if source.Type != StreamSourceDisk {
		t.Fatalf("expected StreamSourceDisk, got %s (reason: %s)", source.Type, source.FallbackReason)
	}
	if source.FallbackReason != FallbackReasonNone {
		t.Fatalf("expected no fallback reason, got %s", source.FallbackReason)
	}
	if source.Length != 2048 {
		t.Fatalf("expected length 2048, got %d", source.Length)
	}

	// Verify reading from source
	buf := make([]byte, 10)
	n, err := source.Reader.Read(buf)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if n != 10 || !bytes.Equal(buf, data[:10]) {
		t.Fatalf("read unexpected data: got %v, want %v", buf, data[:10])
	}

	// Verify seek
	pos, err := source.Reader.Seek(100, 0)
	if err != nil {
		t.Fatalf("seek failed: %v", err)
	}
	if pos != 100 {
		t.Fatalf("expected pos 100, got %d", pos)
	}
	n, err = source.Reader.Read(buf)
	if err != nil {
		t.Fatalf("read after seek failed: %v", err)
	}
	if n != 10 || !bytes.Equal(buf, data[100:110]) {
		t.Fatalf("read unexpected data: got %v, want %v", buf, data[100:110])
	}
}

func TestStreamSource_PartFileExistsBlocksDisk(t *testing.T) {
	pieceLen := int64(1024)
	data := make([]byte, 2048)
	files := []metainfo.FileInfo{
		{Length: 2048, Path: []string{"movie.mp4"}},
	}
	e, tor, dir := setupEngineWithTorrent(t, pieceLen, "test_part", files, [][]byte{data})

	targetDir := filepath.Join(dir, "test_part")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	targetFile := filepath.Join(targetDir, "movie.mp4")
	if err := os.WriteFile(targetFile, data, 0o644); err != nil {
		t.Fatal(err)
	}

	verifyEngineTorrentPieces(t, e, tor.Hash, 0, 2)

	// Add .part file
	if err := os.WriteFile(targetFile+".part", []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	source, err := e.OpenStreamSource(ctx, tor.Hash, 0, false)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	defer source.Close()

	if source.Type != StreamSourceTorrent {
		t.Fatalf("expected StreamSourceTorrent, got %s", source.Type)
	}
	if source.FallbackReason != FallbackReasonNotReady {
		t.Fatalf("expected revoked verified readiness, got %s", source.FallbackReason)
	}
}

func TestStreamSource_SizeMismatchBlocksDisk(t *testing.T) {
	pieceLen := int64(1024)
	data := make([]byte, 2048)
	files := []metainfo.FileInfo{
		{Length: 2048, Path: []string{"movie.mp4"}},
	}
	e, tor, dir := setupEngineWithTorrent(t, pieceLen, "test_size", files, [][]byte{data})

	targetDir := filepath.Join(dir, "test_size")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	targetFile := filepath.Join(targetDir, "movie.mp4")
	if err := os.WriteFile(targetFile, data, 0o644); err != nil {
		t.Fatal(err)
	}

	verifyEngineTorrentPieces(t, e, tor.Hash, 0, 2)

	// Make file writable, then truncate file after verification
	_ = os.Chmod(targetFile, 0o666)
	if err := os.Truncate(targetFile, 1000); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	source, err := e.OpenStreamSource(ctx, tor.Hash, 0, false)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	defer source.Close()

	if source.Type != StreamSourceTorrent {
		t.Fatalf("expected StreamSourceTorrent, got %s", source.Type)
	}
	if source.FallbackReason != FallbackReasonNotReady {
		t.Fatalf("expected revoked verified readiness, got %s", source.FallbackReason)
	}
}

func TestStreamSource_DirectoryInsteadOfFile(t *testing.T) {
	pieceLen := int64(1024)
	data := make([]byte, 2048)
	files := []metainfo.FileInfo{
		{Length: 2048, Path: []string{"folder_as_file"}},
	}
	e, tor, dir := setupEngineWithTorrent(t, pieceLen, "test_dir", files, [][]byte{data})

	targetDir := filepath.Join(dir, "test_dir")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	targetFile := filepath.Join(targetDir, "folder_as_file")
	if err := os.WriteFile(targetFile, data, 0o644); err != nil {
		t.Fatal(err)
	}
	verifyEngineTorrentPieces(t, e, tor.Hash, 0, 2)

	// Make file writable, then replace file with directory
	_ = os.Chmod(targetFile, 0o666)
	if err := os.Remove(targetFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(targetFile, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	source, err := e.OpenStreamSource(ctx, tor.Hash, 0, false)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	defer source.Close()

	if source.Type != StreamSourceTorrent {
		t.Fatalf("expected StreamSourceTorrent, got %s", source.Type)
	}
	if source.FallbackReason != FallbackReasonNotReady {
		t.Fatalf("expected revoked verified readiness, got %s", source.FallbackReason)
	}
}

func TestStreamSource_UnicodeAndMultiFileSameBasename(t *testing.T) {
	pieceLen := int64(1000)
	s1Data := bytes.Repeat([]byte("1"), 1000)
	s2Data := bytes.Repeat([]byte("2"), 1000)
	files := []metainfo.FileInfo{
		{Length: 1000, Path: []string{"Сезон 1", "эпизод.mkv"}},
		{Length: 1000, Path: []string{"Сезон 2", "эпизод.mkv"}},
	}
	e, tor, dir := setupEngineWithTorrent(t, pieceLen, "Сериал 🎉", files, [][]byte{s1Data, s2Data})

	s1Dir := filepath.Join(dir, "Сериал 🎉", "Сезон 1")
	s2Dir := filepath.Join(dir, "Сериал 🎉", "Сезон 2")
	if err := os.MkdirAll(s1Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s2Dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Only write Season 1 file to disk
	if err := os.WriteFile(filepath.Join(s1Dir, "эпизод.mkv"), s1Data, 0o644); err != nil {
		t.Fatal(err)
	}

	// Verify only piece 0 (Season 1)
	verifyEngineTorrentPieces(t, e, tor.Hash, 0, 1)

	ctx := context.Background()
	src1, err := e.OpenStreamSource(ctx, tor.Hash, 0, false)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	defer src1.Close()

	if src1.Type != StreamSourceDisk {
		t.Fatalf("expected StreamSourceDisk, got %s (reason: %s)", src1.Type, src1.FallbackReason)
	}
	buf := make([]byte, 10)
	if _, err := src1.Reader.Read(buf); err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if !bytes.Equal(buf, []byte("1111111111")) {
		t.Fatalf("unexpected data: %s", string(buf))
	}

	src2, err := e.OpenStreamSource(ctx, tor.Hash, 1, false)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	defer src2.Close()
	if src2.Type != StreamSourceTorrent {
		t.Fatalf("expected StreamSourceTorrent, got %s", src2.Type)
	}
	if src2.FallbackReason != FallbackReasonNotReady {
		t.Fatalf("expected FallbackReasonNotReady, got %s", src2.FallbackReason)
	}
}

func TestStreamSource_HEADRequest(t *testing.T) {
	pieceLen := int64(1024)
	data := make([]byte, 2048)
	files := []metainfo.FileInfo{
		{Length: 2048, Path: []string{"video.mp4"}},
	}
	e, tor, _ := setupEngineWithTorrent(t, pieceLen, "test_head", files, [][]byte{data})

	// Incomplete file HEAD
	ctx := context.Background()
	src, err := e.OpenStreamSource(ctx, tor.Hash, 0, true)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	defer src.Close()

	if src.Length != 2048 {
		t.Fatalf("expected length 2048, got %d", src.Length)
	}
	pos, err := src.Reader.Seek(0, 2)
	if err != nil {
		t.Fatalf("seek failed: %v", err)
	}
	if pos != 2048 {
		t.Fatalf("expected pos 2048, got %d", pos)
	}
}

func TestStreamSource_ContextCancelledAndTorrentDeleted(t *testing.T) {
	pieceLen := int64(1024)
	data := make([]byte, 2048)
	files := []metainfo.FileInfo{
		{Length: 2048, Path: []string{"video.mp4"}},
	}
	e, tor, dir := setupEngineWithTorrent(t, pieceLen, "test_cancel", files, [][]byte{data})

	targetDir := filepath.Join(dir, "test_cancel")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "video.mp4"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	verifyEngineTorrentPieces(t, e, tor.Hash, 0, 2)

	// Cancel context before open
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.OpenStreamSource(canceledCtx, tor.Hash, 0, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// Open with live context, then cancel during read
	liveCtx, cancelLive := context.WithCancel(context.Background())
	src, err := e.OpenStreamSource(liveCtx, tor.Hash, 0, false)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	defer src.Close()

	cancelLive()
	buf := make([]byte, 10)
	_, err = src.Reader.Read(buf)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled on read, got %v", err)
	}

	// Test torrent delete terminates stream
	src2, err := e.OpenStreamSource(context.Background(), tor.Hash, 0, false)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	defer src2.Close()

	if err := e.Delete(tor.Hash); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	_, err = src2.Reader.Read(buf)
	if err == nil {
		t.Fatal("expected error after torrent delete, got nil")
	}
}

func TestStreamSource_ConcurrentDownloadAndDirectStream(t *testing.T) {
	// Torrent A: Completed, 4 KiB file
	dataA := bytes.Repeat([]byte("AlphaStreamData!"), 256) // 4096 bytes
	e, torA, dirA := setupEngineWithTorrent(t, 1024, "torrent_a", []metainfo.FileInfo{
		{Length: int64(len(dataA)), Path: []string{"movie_a.mp4"}},
	}, [][]byte{dataA})

	filePathA := filepath.Join(dirA, "torrent_a", "movie_a.mp4")
	if err := os.MkdirAll(filepath.Dir(filePathA), 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	if err := os.WriteFile(filePathA, dataA, 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	verifyEngineTorrentPieces(t, e, torA.Hash, 0, 4)

	// Setup independent Seeder for Torrent B
	dataB := bytes.Repeat([]byte("BetaDownloadData"), 1024) // 16384 bytes
	seederDir := t.TempDir()
	seederCfg := torrent.TestingConfig(t)
	seederCfg.Seed = true
	seederCfg.MaxAllocPeerRequestDataPerConn = 1024 * 1024
	seederCfg.DataDir = seederDir
	seederClient, err := torrent.NewClient(seederCfg)
	if err != nil {
		t.Fatalf("failed to create seeder client: %v", err)
	}
	defer seederClient.Close()

	torrentBytesB := createTestTorrentBytes(1024, "torrent_b", []metainfo.FileInfo{
		{Length: int64(len(dataB)), Path: []string{"data_b.bin"}},
	}, [][]byte{dataB})

	// Seed on seederClient
	seederFilePath := filepath.Join(seederDir, "torrent_b", "data_b.bin")
	if err := os.MkdirAll(filepath.Dir(seederFilePath), 0o755); err != nil {
		t.Fatalf("failed to create seeder dir: %v", err)
	}
	if err := os.WriteFile(seederFilePath, dataB, 0o644); err != nil {
		t.Fatalf("failed to write seeder file: %v", err)
	}
	miB, err := metainfo.Load(bytes.NewReader(torrentBytesB))
	if err != nil {
		t.Fatalf("failed to load mi: %v", err)
	}
	seederTor, _, err := seederClient.AddTorrentSpec(torrent.TorrentSpecFromMetaInfo(miB))
	if err != nil {
		t.Fatalf("failed to add seeder torrent: %v", err)
	}
	seederTor.VerifyData()
	deadlineSeeder := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadlineSeeder) {
		if seederTor.BytesCompleted() == int64(len(dataB)) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if seederTor.BytesCompleted() != int64(len(dataB)) {
		t.Fatalf("seeder failed to verify data: completed %d want %d", seederTor.BytesCompleted(), len(dataB))
	}

	// Allow downloads on engine
	e.cfg.MaxActiveDownloads = 5

	// Add Torrent B to our engine as a downloading torrent
	torB, err := e.AddTorrentFile(bytes.NewReader(torrentBytesB))
	if err != nil {
		t.Fatalf("failed to add torrent B to engine: %v", err)
	}

	e.mu.RLock()
	mtB := e.managedTorrents[torB.Hash]
	e.mu.RUnlock()

	// Connect Torrent B to seeder and start download
	mtB.t.AddClientPeer(seederClient)
	mtB.t.DownloadAll()

	// Open stream source for Torrent A (Disk source)
	srcA, err := e.OpenStreamSource(context.Background(), torA.Hash, 0, false)
	if err != nil {
		t.Fatalf("failed to open stream A: %v", err)
	}
	defer srcA.Close()

	if srcA.Type != StreamSourceDisk {
		t.Fatalf("expected stream A to use StreamSourceDisk, got %s", srcA.Type)
	}

	// Concurrently read Torrent A and wait for Torrent B download progress
	streamDone := make(chan struct{})
	var streamedData []byte
	go func() {
		buf := make([]byte, 256)
		for {
			n, rErr := srcA.Reader.Read(buf)
			if n > 0 {
				streamedData = append(streamedData, buf[:n]...)
			}
			if rErr == io.EOF {
				break
			}
			if rErr != nil {
				t.Errorf("read error on stream A: %v", rErr)
				break
			}
			time.Sleep(1 * time.Millisecond) // simulate realistic streaming pace
		}
		close(streamDone)
	}()

	// Wait for Stream A to finish
	select {
	case <-streamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("stream A timed out")
	}

	if !bytes.Equal(streamedData, dataA) {
		t.Fatalf("stream A data corruption: got %d bytes, want %d", len(streamedData), len(dataA))
	}

	// Verify Torrent B made progress
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if mtB.t.BytesCompleted() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if mtB.t.BytesCompleted() == 0 {
		t.Fatal("expected Torrent B to download data from loopback seeder")
	}
}
