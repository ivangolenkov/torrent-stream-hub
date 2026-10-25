package usecase

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"torrent-stream-hub/internal/completion"
	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
)

// Reproducible local-FS measurement, intentionally outside normal test runs.
// A fixed 1 ms read callback emulates a contended disk, not NAS throughput.
// Run -bench=BenchmarkVerificationStartup -benchtime=1x -count=3 before/after
// scheduler changes. HTTP measures the shared list projection during restore.
func BenchmarkVerificationStartup(b *testing.B) {
	for iteration := 0; iteration < b.N; iteration++ {
		dir := b.TempDir()
		cfg := &config.Config{DBPath: filepath.Join(dir, "hub.db"), DownloadDir: dir, BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true, BTDisableTCP: true, BTDisableUTP: true, BTRetrackersMode: "off", MaxActiveDownloads: 4}
		db, err := repository.NewSQLiteDB(cfg.DBPath)
		if err != nil {
			b.Fatal(err)
		}
		repo := repository.NewTorrentRepo(db)
		first, err := engine.New(cfg)
		if err != nil {
			b.Fatal(err)
		}
		initial := NewTorrentUseCase(first, repo)
		const pieceLength = 64 << 10
		data := bytes.Repeat([]byte("x"), 2<<20)
		digest := sha1.Sum(data[:pieceLength])
		var hashes []string
		for index := 0; index < 4; index++ {
			info := metainfo.Info{Name: fmt.Sprintf("fixture-%d.bin", index), Length: int64(len(data)), PieceLength: pieceLength, Pieces: bytes.Repeat(digest[:], len(data)/pieceLength)}
			encoded, err := bencode.Marshal(info)
			if err != nil {
				b.Fatal(err)
			}
			var input bytes.Buffer
			if err := (&metainfo.MetaInfo{InfoBytes: encoded}).Write(&input); err != nil {
				b.Fatal(err)
			}
			tor, err := initial.AddTorrentFile(&input)
			if err != nil {
				b.Fatal(err)
			}
			if err := initial.Pause(tor.Hash); err != nil {
				b.Fatal(err)
			}
			if err := first.Delete(tor.Hash); err != nil {
				b.Fatal(err)
			}
			mapping, err := repo.StorageMapping(tor.Hash)
			if err != nil {
				b.Fatal(err)
			}
			path := filepath.Join(dir, mapping.Root, info.Name)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				b.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				b.Fatal(err)
			}
			intent, err := repo.GetIntent(tor.Hash)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := repo.CommitRunIntent(intent.Version, models.RunEnabled); err != nil {
				b.Fatal(err)
			}
			hashes = append(hashes, tor.Hash)
		}
		first.Close()
		pc, err := completion.New(db.DB())
		if err != nil {
			b.Fatal(err)
		}
		if _, err := db.DB().Exec(`CREATE TABLE measurement_writes(n INTEGER); INSERT INTO measurement_writes VALUES(0)`); err != nil {
			b.Fatal(err)
		}
		for _, table := range []string{"torrents", "files", "torrent_intents", "torrent_storage", "storage_inventory", "file_inventory", "piece_completion"} {
			for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
				if _, err := db.DB().Exec(fmt.Sprintf(`CREATE TRIGGER measure_%s_%s AFTER %s ON %s BEGIN UPDATE measurement_writes SET n=n+1; END`, table, event, event, table)); err != nil {
					b.Fatal(err)
				}
			}
		}
		var readBytes, opens, stats, resourceOpens, resourceStats, active, maxActive atomic.Int64
		gate := make(chan struct{})
		var gateClosed atomic.Bool
		closeGate := func() {
			if gateClosed.CompareAndSwap(false, true) {
				close(gate)
			}
		}
		defer closeGate()
		observe := func(op, path string, n int64) {
			switch op {
			case "resource-open":
				resourceOpens.Add(1)
			case "resource-stat", "resource-lstat", "resource-fstat":
				resourceStats.Add(1)
			case "open":
				opens.Add(1)
			case "stat", "lstat", "fstat":
				stats.Add(1)
			case "read":
				readBytes.Add(n)
				a := active.Add(1)
				for old := maxActive.Load(); a > old; old = maxActive.Load() {
					if maxActive.CompareAndSwap(old, a) {
						break
					}
				}
				<-gate
				time.Sleep(time.Millisecond)
				active.Add(-1)
			}
		}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		started := time.Now()
		eng, err := engine.New(cfg, engine.WithPieceCompletion(pc), engine.WithStorageIOObserver(observe))
		if err != nil {
			b.Fatal(err)
		}
		defer func() { closeGate(); eng.Close(); db.Close() }()
		uc := NewTorrentUseCase(eng, repo)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			torrents, err := uc.GetAllTorrents()
			if err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(CompactTorrents(torrents))
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		restored := make(chan error, 1)
		go func() { restored <- uc.RestoreTorrents() }()
		for {
			response, err := http.Get(server.URL)
			if err != nil {
				b.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == 200 {
				break
			}
			select {
			case <-ctx.Done():
				b.Fatal(ctx.Err())
			case <-time.After(time.Millisecond):
			}
		}
		apiReady := time.Since(started)
		select {
		case err := <-restored:
			if err != nil {
				b.Fatal(err)
			}
		case <-ctx.Done():
			b.Fatal(ctx.Err())
		}
		source, err := uc.OpenStreamSource(ctx, hashes[len(hashes)-1], 0, false)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := source.Reader.Seek(int64(len(data)-pieceLength), io.SeekStart); err != nil {
			b.Fatal(err)
		}
		readDone := make(chan error, 1)
		go func() {
			out := make([]byte, 16)
			_, err := io.ReadFull(source.Reader, out)
			if err == nil && !bytes.Equal(out, data[:16]) {
				err = fmt.Errorf("unverified range")
			}
			readDone <- err
		}()
		// Give the Reader a deterministic chance to publish its range before
		// releasing blocked hashers; this is synchronization, not a latency target.
		target := eng.GetRawTorrent(hashes[len(hashes)-1])
		for target.FileObservations()[0].ReaderPieces == 0 {
			select {
			case <-ctx.Done():
				b.Fatal(ctx.Err())
			case <-time.After(time.Millisecond):
			}
		}
		closeGate()
		select {
		case err := <-readDone:
			if err != nil {
				b.Fatal(err)
			}
		case <-ctx.Done():
			b.Fatal(ctx.Err())
		}
		firstRange := time.Since(started)
		source.Close()
		for _, hash := range hashes {
			select {
			case <-eng.GetRawTorrent(hash).Complete().On():
			case <-ctx.Done():
				b.Fatal(ctx.Err())
			}
		}
		finished := time.Since(started)
		server.Close()
		cancel()
		eng.Close()
		runtime.ReadMemStats(&after)
		var writes int64
		if err := db.DB().QueryRow(`SELECT n FROM measurement_writes`).Scan(&writes); err != nil {
			b.Fatal(err)
		}
		db.Close()
		b.ReportMetric(float64(apiReady.Microseconds())/1000, "api-ms")
		b.ReportMetric(float64(firstRange.Microseconds())/1000, "range-ms")
		b.ReportMetric(float64(finished.Microseconds())/1000, "complete-ms")
		b.ReportMetric(float64(readBytes.Load()), "payload-read-B")
		b.ReportMetric(float64(opens.Load()), "payload-opens")
		b.ReportMetric(float64(stats.Load()), "payload-stats")
		b.ReportMetric(float64(resourceOpens.Load()), "resource-opens")
		b.ReportMetric(float64(resourceStats.Load()), "resource-stats")
		budget := eng.VerificationBudgetStats()
		b.ReportMetric(float64(budget.PeakActive), "hash-concurrency")
		b.ReportMetric(float64(budget.PeakPending), "hash-queue")
		b.ReportMetric(float64(maxActive.Load()), "read-concurrency")
		b.ReportMetric(float64(after.TotalAlloc-before.TotalAlloc), "allocated-B")
		b.ReportMetric(float64(writes), "SQL-row-writes")
	}
}
