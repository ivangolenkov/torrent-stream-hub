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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"torrent-stream-hub/internal/completion"
	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/repository"
)

// Reproducible local-FS measurement, intentionally outside normal test runs.
// A fixed 1 ms read callback emulates a contended disk, not NAS throughput.
// Run -bench=BenchmarkVerificationStartup -benchtime=1x -count=3 before/after
// scheduler changes. HTTP measures the shared list projection during restore.
func BenchmarkVerificationStartup(b *testing.B) {
	benchmarkVerificationStartup(b, false, false)
}

// Baseline for phase 43: verify a real first session, close storage/completion
// and SQLite, then reopen. The mixed library has two 50%-valid torrents and two
// complete ones; the reader targets a valid piece of a partial torrent.
func BenchmarkFastResumeBaseline(b *testing.B) {
	b.Run("complete", func(b *testing.B) { benchmarkVerificationStartup(b, true, false) })
	b.Run("mixed", func(b *testing.B) { benchmarkVerificationStartup(b, true, true) })
}

func BenchmarkFastResumeResult(b *testing.B) {
	b.Run("complete", func(b *testing.B) { benchmarkVerificationStartup(b, true, false, true) })
	b.Run("mixed", func(b *testing.B) { benchmarkVerificationStartup(b, true, true, true) })
}

func benchmarkVerificationStartup(b *testing.B, cleanReopen, mixed bool, fastMode ...bool) {
	b.Helper()
	fast := len(fastMode) != 0 && fastMode[0]
	for iteration := 0; iteration < b.N; iteration++ {
		dir := b.TempDir()
		cfg := &config.Config{DBPath: filepath.Join(dir, "hub.db"), DownloadDir: dir, BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true, BTDisableTCP: true, BTDisableUTP: true, BTRetrackersMode: "off", MaxActiveDownloads: 4}
		cfg.FastResumeMode = "verify"
		if fast {
			cfg.FastResumeMode = "fast"
		}
		// Keep the WAL append-only during this controlled measurement. Each
		// commit frame records an actual committed SQLite write transaction;
		// this is not a guess from trigger row counts or SQL API calls.
		measurementDSN := cfg.DBPath + "?_pragma=wal_autocheckpoint(0)"
		db, err := repository.NewSQLiteDB(measurementDSN)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = db.Close() })
		repo := repository.NewTorrentRepo(db)
		var initialOptions []engine.Option
		if cleanReopen {
			initialCompletion, err := completion.New(db.DB())
			if err != nil {
				b.Fatal(err)
			}
			initialOptions = append(initialOptions, engine.WithPieceCompletion(initialCompletion))
		}
		first, err := engine.New(cfg, initialOptions...)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(first.Close)
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
			payload := bytes.Clone(data)
			if mixed && index%2 == 1 {
				// Keep the final half valid so the same tail Range is readable.
				clear(payload[:len(payload)/2])
			}
			if err := os.WriteFile(path, payload, 0600); err != nil {
				b.Fatal(err)
			}
			if cleanReopen {
				saved, err := repo.GetTorrent(tor.Hash)
				if err != nil {
					b.Fatal(err)
				}
				if ready, err := initial.inspectInventory(context.Background(), tor.Hash, false); err != nil || !ready {
					b.Fatal(ready, err)
				}
				if err := initial.restoreInactiveLocked(saved); err != nil {
					b.Fatal(err)
				}
				intent, err := repo.GetIntent(tor.Hash)
				if err != nil {
					b.Fatal(err)
				}
				if err := bindVerificationRuntime(first, repo, intent.Version); err != nil {
					b.Fatal(err)
				}
				raw := first.GetRawTorrent(tor.Hash)
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				err = raw.VerifyDataContext(ctx)
				cancel()
				if err != nil {
					b.Fatal(err)
				}
				want := int64(len(data))
				if mixed && index%2 == 1 {
					want /= 2
				}
				if observed := raw.FileObservations(); len(observed) != 1 || observed[0].VerifiedBytes != want {
					b.Fatalf("initial fixture verification: got %+v, want %d verified bytes", observed, want)
				}
			}
			if err := initial.Resume(tor.Hash); err != nil {
				b.Fatal(err)
			}
			hashes = append(hashes, tor.Hash)
		}
		first.Close()
		if cleanReopen {
			if err := db.Close(); err != nil {
				b.Fatal(err)
			}
			db, err = repository.NewSQLiteDB(measurementDSN)
			if err != nil {
				b.Fatal(err)
			}
			repo = repository.NewTorrentRepo(db)
		}
		pc, err := completion.New(db.DB())
		if err != nil {
			b.Fatal(err)
		}
		if _, err := db.DB().Exec(`CREATE TABLE measurement_writes(n INTEGER); INSERT INTO measurement_writes VALUES(0)`); err != nil {
			b.Fatal(err)
		}
		for _, table := range []string{"torrents", "files", "torrent_intents", "torrent_storage", "storage_inventory", "file_inventory", "piece_completion", "resume_owners", "resume_checkpoints"} {
			for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
				if _, err := db.DB().Exec(fmt.Sprintf(`CREATE TRIGGER measure_%s_%s AFTER %s ON %s BEGIN UPDATE measurement_writes SET n=n+1; END`, table, event, event, table)); err != nil {
					b.Fatal(err)
				}
			}
		}
		walBefore, err := sampleMeasurementDatabaseWAL(db, cfg.DBPath+"-wal")
		if err != nil {
			b.Fatal(err)
		}
		var readBytes, opens, stats, resourceOpens, resourceStats, active, maxActive atomic.Int64
		var payloadSyncs, directorySyncs atomic.Int64
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
			case "checkpoint-payload-sync-after":
				payloadSyncs.Add(1)
			case "checkpoint-directory-sync-after":
				directorySyncs.Add(1)
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
		peakHeap, stopHeapSample := sampleVerificationHeap(before.HeapAlloc)
		b.Cleanup(stopHeapSample)
		started := time.Now()
		resumeTraceMarker(b, "begin")
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
		restoreFinished := time.Since(started)
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
		for !fast && target.FileObservations()[0].ReaderPieces == 0 {
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
		for index, hash := range hashes {
			wantChecked := int64(len(data))
			if fast {
				wantChecked = 0
				if mixed && index%2 == 1 {
					wantChecked = int64(len(data) / 2)
				}
			}
			for {
				observed := eng.GetRawTorrent(hash).FileObservations()
				if len(observed) == 1 && observed[0].CheckedBytes == wantChecked && observed[0].QueuedPieces == 0 && observed[0].ActivePieces == 0 {
					want := int64(len(data))
					if mixed && index%2 == 1 {
						want /= 2
					}
					if observed[0].VerifiedBytes != want {
						b.Fatalf("restored fixture verification: got %+v, want %d verified bytes", observed, want)
					}
					break
				}
				select {
				case <-ctx.Done():
					b.Fatal(ctx.Err())
				case <-time.After(time.Millisecond):
				}
			}
		}
		finished := time.Since(started)
		server.Close()
		cancel()
		walAfterRestore, err := sampleMeasurementDatabaseWAL(db, cfg.DBPath+"-wal")
		if err != nil {
			b.Fatal(err)
		}
		sealStarted := time.Now()
		resumeTraceMarker(b, "online-seal-begin")
		for _, hash := range hashes {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			release, err := uc.lockRecord(hash, ctx)
			if err != nil {
				cancel()
				b.Fatal(err)
			}
			m, err := repo.StorageMapping(hash)
			if err == nil {
				_, mapping, source, sourceErr := uc.productionResumeSource(ctx, m.StorageID)
				err = sourceErr
				if err == nil {
					err = repo.SealResume(ctx, mapping, source)
				}
			}
			release()
			cancel()
			if err != nil {
				b.Fatal(err)
			}
		}
		onlineSeal := time.Since(sealStarted)
		resumeTraceMarker(b, "online-seal-end")
		walAfterSeal, err := sampleMeasurementDatabaseWAL(db, cfg.DBPath+"-wal")
		if err != nil {
			b.Fatal(err)
		}
		shutdownStarted := time.Now()
		eng.Close()
		shutdown := time.Since(shutdownStarted)
		resumeTraceMarker(b, "end")
		stopHeapSample()
		runtime.ReadMemStats(&after)
		var writes int64
		if err := db.DB().QueryRow(`SELECT n FROM measurement_writes`).Scan(&writes); err != nil {
			b.Fatal(err)
		}
		walAfterClose, err := sampleMeasurementDatabaseWAL(db, cfg.DBPath+"-wal")
		if err != nil {
			b.Fatal(err)
		}
		if walBefore.Salt != walAfterRestore.Salt || walBefore.Salt != walAfterSeal.Salt || walBefore.Salt != walAfterClose.Salt {
			b.Fatal("WAL restarted during measurement")
		}
		db.Close()
		b.ReportMetric(float64(apiReady.Microseconds())/1000, "api-ms")
		b.ReportMetric(float64(firstRange.Microseconds())/1000, "range-ms")
		b.ReportMetric(float64(restoreFinished.Microseconds())/1000, "restore-ms")
		b.ReportMetric(float64(finished.Microseconds())/1000, "complete-ms")
		b.ReportMetric(float64(shutdown.Microseconds())/1000, "shutdown-ms")
		b.ReportMetric(float64(onlineSeal.Microseconds())/1000, "online-seal-4-owners-ms")
		b.ReportMetric(float64(walAfterRestore.Commits-walBefore.Commits), "restore-write-tx")
		b.ReportMetric(float64(walAfterSeal.Commits-walAfterRestore.Commits), "online-seal-write-tx")
		b.ReportMetric(float64(walAfterClose.Commits-walAfterSeal.Commits), "shutdown-write-tx")
		b.ReportMetric(float64(walAfterClose.Commits-walBefore.Commits), "total-write-tx")
		b.ReportMetric(float64(walAfterClose.Bytes-walBefore.Bytes), "WAL-appended-B")
		b.ReportMetric(float64(payloadSyncs.Load()), "checkpoint-payload-syncs")
		b.ReportMetric(float64(directorySyncs.Load()), "checkpoint-directory-syncs")
		b.ReportMetric(float64(readBytes.Load()), "payload-read-B")
		hashBytes := readBytes.Load()
		if source.Type == engine.StreamSourceTorrent {
			hashBytes -= 16
		}
		b.ReportMetric(float64(hashBytes), "hash-payload-B")
		b.ReportMetric(16, "actual-range-B")
		b.ReportMetric(float64(opens.Load()), "payload-opens")
		b.ReportMetric(float64(stats.Load()), "payload-stats")
		b.ReportMetric(float64(resourceOpens.Load()), "resource-opens")
		b.ReportMetric(float64(resourceStats.Load()), "resource-stats")
		budget := eng.VerificationBudgetStats()
		b.ReportMetric(float64(budget.PeakActive), "hash-concurrency")
		b.ReportMetric(float64(budget.PeakPending), "hash-queue")
		b.ReportMetric(float64(maxActive.Load()), "read-concurrency")
		b.ReportMetric(float64(after.TotalAlloc-before.TotalAlloc), "allocated-B")
		b.ReportMetric(float64(peakHeap.Load()), "sampled-heap-B")
		b.ReportMetric(float64(writes), "SQL-row-writes")
	}
}

// Optional wall-clock boundaries for an external Linux strace run. They are not
// sync counters: syscall counts must come from the tracer, not WAL arithmetic.
func resumeTraceMarker(b *testing.B, stage string) {
	if os.Getenv("TSH_RESUME_TRACE") == "1" {
		fmt.Fprintf(os.Stderr, "TSH_RESUME_MEASURE name=%s stage=%s unix_ns=%d\n", b.Name(), stage, time.Now().UnixNano())
	}
}

// Sampled process Go heap, not RSS or a guaranteed instantaneous maximum. Keep
// the sampler identical in before/after runs and exclude fixture preparation.
func sampleVerificationHeap(initial uint64) (*atomic.Uint64, func()) {
	peak := &atomic.Uint64{}
	peak.Store(initial)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				for old := peak.Load(); m.HeapAlloc > old; old = peak.Load() {
					if peak.CompareAndSwap(old, m.HeapAlloc) {
						break
					}
				}
			}
		}
	}()
	var once sync.Once
	return peak, func() { once.Do(func() { close(stop); <-done }) }
}
