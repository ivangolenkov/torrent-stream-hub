package torrserver

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/repository"
	"torrent-stream-hub/internal/usecase"

	"github.com/anacrolix/torrent/metainfo"
)

func setupBenchmarkEngine(b *testing.B, totalSize int64, pieceLength int64) (*usecase.TorrentUseCase, string, []byte, func()) {
	b.Helper()
	dir := b.TempDir()
	db, err := repository.NewSQLiteDB(filepath.Join(dir, "bench.db"))
	if err != nil {
		b.Fatalf("failed to create DB: %v", err)
	}

	eng, err := engine.New(&config.Config{
		DownloadDir:  dir,
		TorrentPort:  0,
		BTDisableDHT: true,
		BTDisablePEX: true,
		BTNoUpload:   true,
	})
	if err != nil {
		db.Close()
		b.Fatalf("failed to create engine: %v", err)
	}

	repo := repository.NewTorrentRepo(db)
	uc := usecase.NewTorrentUseCase(eng, repo)

	fileData := make([]byte, totalSize)
	for i := range fileData {
		fileData[i] = byte(i % 251)
	}

	torrentBytes := createTorrserverTestTorrent(pieceLength, "bench_file", []metainfo.FileInfo{
		{Length: totalSize, Path: []string{"bench_file.mkv"}},
	}, [][]byte{fileData})

	tor, err := uc.AddTorrentFile(bytes.NewReader(torrentBytes))
	if err != nil {
		eng.Close()
		db.Close()
		b.Fatalf("failed to add torrent: %v", err)
	}
	hash := tor.Hash

	filePath := filepath.Join(dir, "payload", hash, "bench_file", "bench_file.mkv")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		eng.Close()
		db.Close()
		b.Fatalf("failed to create dir: %v", err)
	}
	if err := os.WriteFile(filePath, fileData, 0o644); err != nil {
		eng.Close()
		db.Close()
		b.Fatalf("failed to write file: %v", err)
	}

	tf, err := uc.GetTorrentFile(hash, 0)
	if err != nil {
		eng.Close()
		db.Close()
		b.Fatalf("failed to get torrent file: %v", err)
	}
	tRaw := tf.Torrent()
	for i := 0; i < tRaw.NumPieces(); i++ {
		_ = tRaw.Piece(i).VerifyData()
	}
	deadline := time.Now().Add(5 * time.Second)
	for i := 0; i < tRaw.NumPieces(); i++ {
		p := tRaw.Piece(i)
		for p.State().Marking && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
	}

	cleanup := func() {
		eng.Close()
		db.Close()
	}

	return uc, hash, fileData, cleanup
}

func BenchmarkStream_DiskSource(b *testing.B) {
	const totalSize = 4 << 20 // 4 MiB
	uc, hash, _, cleanup := setupBenchmarkEngine(b, totalSize, 256<<10)
	defer cleanup()

	ctx := context.Background()
	buf := make([]byte, 64<<10) // 64 KiB buffer per read

	b.SetBytes(totalSize)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		src, err := uc.OpenStreamSource(ctx, hash, 0, false)
		if err != nil {
			b.Fatalf("OpenStreamSource failed: %v", err)
		}
		if src.Type != engine.StreamSourceDisk {
			src.Close()
			b.Fatalf("expected disk source, got %s", src.Type)
		}

		var totalRead int64
		for {
			n, rErr := src.Reader.Read(buf)
			totalRead += int64(n)
			if rErr == io.EOF {
				break
			}
			if rErr != nil {
				src.Close()
				b.Fatalf("read failed: %v", rErr)
			}
		}
		if totalRead != totalSize {
			src.Close()
			b.Fatalf("expected to read %d bytes, got %d", totalSize, totalRead)
		}
		src.Close()
	}
}

func BenchmarkStream_TorrentSource(b *testing.B) {
	const totalSize = 4 << 20 // 4 MiB
	uc, hash, _, cleanup := setupBenchmarkEngine(b, totalSize, 256<<10)
	defer cleanup()

	tf, err := uc.GetTorrentFile(hash, 0)
	if err != nil {
		b.Fatalf("GetTorrentFile failed: %v", err)
	}

	buf := make([]byte, 64<<10) // 64 KiB buffer per read

	b.SetBytes(totalSize)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		reader := tf.NewReader()
		reader.SetResponsive()

		var totalRead int64
		for {
			n, rErr := reader.Read(buf)
			totalRead += int64(n)
			if rErr == io.EOF {
				break
			}
			if rErr != nil {
				reader.Close()
				b.Fatalf("read failed: %v", rErr)
			}
		}
		if totalRead != totalSize {
			reader.Close()
			b.Fatalf("expected to read %d bytes, got %d", totalSize, totalRead)
		}
		reader.Close()
	}
}
