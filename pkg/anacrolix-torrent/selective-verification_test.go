package torrent

import (
	"bytes"
	"context"
	"crypto/sha1"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

type completionFailureBackend struct {
	storage.ClientImpl
	fail *atomic.Bool
}

func (b completionFailureBackend) OpenTorrent(ctx context.Context, info *metainfo.Info, hash metainfo.Hash) (storage.TorrentImpl, error) {
	ts, err := b.ClientImpl.OpenTorrent(ctx, info, hash)
	if err != nil {
		return ts, err
	}
	piece := ts.Piece
	ts.Piece = func(p metainfo.Piece) storage.PieceImpl {
		return completionFailurePiece{PieceImpl: piece(p), fail: b.fail}
	}
	return ts, nil
}

type completionFailurePiece struct {
	storage.PieceImpl
	fail *atomic.Bool
}

func (p completionFailurePiece) BeginVerification() error {
	return p.PieceImpl.(interface{ BeginVerification() error }).BeginVerification()
}
func (p completionFailurePiece) MarkComplete() error {
	if p.fail.Load() {
		return io.ErrClosedPipe
	}
	return p.PieceImpl.MarkComplete()
}

func TestSelectiveVerificationFailedCommitDoesNotAnnounceCompletion(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("good"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	backend := storage.NewFileOpts(storage.NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, UsePartFiles: g.Some(false), NoCompletionRename: true, PieceCompletion: storage.NewMapPieceCompletion()})
	defer backend.Close()
	fail := &atomic.Bool{}
	fail.Store(true)
	cfg := TestingConfig(t)
	cfg.DefaultStorage = completionFailureBackend{backend, fail}
	cfg.DisableTCP, cfg.DisableUTP, cfg.DisableWebtorrent = true, true, true
	cl, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	hash := sha1.Sum([]byte("good"))
	encoded, err := bencode.Marshal(metainfo.Info{Name: "data", Length: 4, PieceLength: 4, Pieces: hash[:]})
	if err != nil {
		t.Fatal(err)
	}
	spec := TorrentSpecFromMetaInfo(&metainfo.MetaInfo{InfoBytes: encoded})
	spec.DisableNetworking, spec.DisallowDataDownload = true, true
	tor, _, err := cl.AddTorrentSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	waitVerificationIdle(t, tor)
	cl.rLock()
	announced := tor._completedPieces.Contains(0)
	cl.rUnlock()
	if announced || tor.PieceState(0).Complete {
		t.Fatal("failed storage commit announced HAVE")
	}
	fail.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := tor.Piece(0).VerifyDataContext(ctx); err != nil {
		t.Fatal(err)
	}
	if !tor.PieceState(0).Complete {
		t.Fatal("successful retry did not complete")
	}
}

func TestSelectiveVerificationLocalSeederHTTPRange(t *testing.T) {
	for _, mode := range []string{"v1", "v2", "hybrid"} {
		t.Run(mode, func(t *testing.T) { selectiveVerificationLocalSeederHTTPRange(t, mode) })
	}
}

func selectiveVerificationLocalSeederHTTPRange(t *testing.T, mode string) {
	t.Helper()
	data := bytes.Repeat([]byte("good"), 8192)
	mi := checkpointMetainfo(t, mode, "video", data)
	seedDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(seedDir, "video"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seedDir, "video", "video.bin"), data, 0600); err != nil {
		t.Fatal(err)
	}
	seedCfg := TestingConfig(t)
	seedCfg.DataDir, seedCfg.Seed = seedDir, true
	seedCfg.DisableUTP, seedCfg.DisableIPv6 = true, true
	seedCfg.MaxAllocPeerRequestDataPerConn = 1 << 20
	seeder, err := NewClient(seedCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer seeder.Close()
	seed, err := seeder.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}
	waitVerificationIdle(t, seed)
	if seed.BytesCompleted() != int64(len(data)) {
		t.Fatal("seeder not verified")
	}
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	backend := storage.NewFileOpts(storage.NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, UsePartFiles: g.Some(false), NoCompletionRename: true, PieceCompletion: storage.NewMapPieceCompletion()})
	defer backend.Close()
	barrier := verificationBarrierStorage{backend, make(chan verificationRequest), make(chan struct{})}
	cfg := TestingConfig(t)
	cfg.DefaultStorage = barrier
	cfg.DisableUTP, cfg.DisableIPv6 = true, true
	cfg.MaxAllocPeerRequestDataPerConn = 1 << 20
	leecher, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer leecher.Close()
	defer close(barrier.stop)
	tor, err := leecher.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}
	if o := tor.FileObservations()[0]; o.ActivePieces != 0 || o.QueuedPieces != 0 || o.CheckedBytes != 0 || o.SkippedMissingBytes != int64(len(data)) {
		t.Fatalf("empty torrent started hash backlog: %+v", o)
	}
	tor.AddClientPeer(seeder)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader := tor.NewReader()
		defer reader.Close()
		reader.SetContext(r.Context())
		reader.SetReadahead(0)
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "video.bin", time.Time{}, reader)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		if err != nil {
			result <- err
			return
		}
		req.Header.Set("Range", "bytes=0-15")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			result <- err
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err == nil && (resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, data[:16])) {
			err = io.ErrUnexpectedEOF
		}
		result <- err
	}()
	first := nextVerification(t, barrier.requests)
	if first.piece != 0 {
		t.Fatalf("range did not prioritize piece zero: %+v", first)
	}
	if tor.PieceState(0).Complete || tor.FileObservations()[0].VerifiedBytes != 0 {
		t.Fatal("received data usable before hashing")
	}
	close(first.proceed)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if tor.BytesCompleted() != 16384 {
		t.Fatalf("range waited for full torrent: %d", tor.BytesCompleted())
	}
}

func TestSelectiveVerificationMissingThenWrite(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	var readBytes atomic.Int64
	backend := storage.NewFileOpts(storage.NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, UsePartFiles: g.Some(false), NoCompletionRename: true, PieceCompletion: storage.NewMapPieceCompletion(), IORead: func(_ string, n int64) { readBytes.Add(n) }})
	defer backend.Close()
	cfg := TestingConfig(t)
	cfg.DefaultStorage = backend
	cfg.DisableTCP, cfg.DisableUTP, cfg.DisableWebtorrent = true, true, true
	cl, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	hash := sha1.Sum([]byte("good"))
	encoded, err := bencode.Marshal(metainfo.Info{Name: "data", Length: 8, PieceLength: 4, Pieces: bytes.Repeat(hash[:], 2)})
	if err != nil {
		t.Fatal(err)
	}
	spec := TorrentSpecFromMetaInfo(&metainfo.MetaInfo{InfoBytes: encoded})
	spec.DisableNetworking, spec.DisallowDataDownload = true, true
	tor, _, err := cl.AddTorrentSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	waitVerificationIdle(t, tor)
	cl.rLock()
	if tor.piece(0).numVerifies != 0 || tor.piece(1).numVerifies != 0 || tor.pieceComplete(0) {
		t.Error("missing data hashed or trusted")
	}
	if tor.storage.InitialIncomplete != nil {
		t.Error("initial evidence retained beyond attach")
	}
	cl.rUnlock()
	if readBytes.Load() != 0 {
		t.Fatal("missing startup read payload")
	}
	if _, err := tor.Piece(0).Storage().WriteAt([]byte("evil"), 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := tor.Piece(0).VerifyDataContext(ctx); err != nil {
		t.Fatal(err)
	}
	if tor.BytesCompleted() != 0 {
		t.Fatal("bad downloaded piece trusted")
	}
	if _, err := tor.Piece(0).Storage().WriteAt([]byte("good"), 0); err != nil {
		t.Fatal(err)
	}
	if err := tor.Piece(0).VerifyDataContext(ctx); err != nil {
		t.Fatal(err)
	}
	if tor.BytesCompleted() != 4 {
		t.Fatalf("new download not verified: %d", tor.BytesCompleted())
	}
	obs := tor.FileObservations()[0]
	if obs.CheckedBytes != 4 || obs.VerifiedBytes != 4 || obs.FreshlyVerifiedBytes != 4 {
		t.Fatalf("skips counted as hashing: %+v", obs)
	}
}

func waitVerificationIdle(tb testing.TB, tor *Torrent) {
	tb.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		tor.cl.rLock()
		idle := tor.activePieceHashes == 0 && tor.piecesQueuedForHash.IsEmpty()
		tor.cl.rUnlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			tb.Fatal("verification did not drain")
		}
		runtime.Gosched()
	}
}

func TestSelectiveVerificationPartialAndSparseVersions(t *testing.T) {
	for _, mode := range []string{"v1", "v2", "hybrid"} {
		for _, sparse := range []bool{false, true} {
			name := mode + "/partial"
			if sparse {
				name = mode + "/sparse"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				data := bytes.Repeat([]byte("good"), 8192)
				path := filepath.Join(dir, "payload", "film", "video.bin")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if sparse {
					f, err := os.Create(path)
					if err != nil {
						t.Fatal(err)
					}
					if err := f.Truncate(int64(len(data))); err != nil {
						f.Close()
						t.Fatal(err)
					}
					if err := f.Close(); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path, data[:16384], 0600); err != nil {
					t.Fatal(err)
				}
				mi := checkpointMetainfo(t, mode, "film", data)
				cl, reads := checkpointClient(t, dir, nil, nil)
				tor := addCheckpointTorrent(t, cl, mi, nil)
				waitVerificationIdle(t, tor)
				o := tor.FileObservations()[0]
				if sparse {
					stats := tor.Stats()
					// SEEK_DATA/HOLE may feed zeroReader to the hasher without
					// physical payload reads. The complete logical extent is hashed.
					if o.CheckedBytes != 32768 || o.SkippedMissingBytes != 0 || o.VerifiedBytes != 0 || stats.BytesHashed.Int64() != 32768 {
						t.Fatalf("logical sparse size granted trust/skipped hash: %+v read=%d", o, reads.Load())
					}
					return
				}
				if o.CheckedBytes != 16384 || o.SkippedMissingBytes != 16384 || o.VerifiedBytes != 16384 || reads.Load() != 16384 {
					t.Fatalf("partial file incorrectly planned: %+v read=%d", o, reads.Load())
				}
				if _, err := tor.Piece(1).Storage().WriteAt(data[16384:], 0); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if err := tor.Piece(1).VerifyDataContext(ctx); err != nil {
					t.Fatal(err)
				}
				if o := tor.FileObservations()[0]; o.VerifiedBytes != 32768 || o.SkippedMissingBytes != 0 || o.CheckedBytes != 32768 {
					t.Fatalf("newly downloaded tail bypassed hashing or retained skip: %+v", o)
				}
			})
		}
	}
}

func BenchmarkSelectiveVerificationStartup(b *testing.B) {
	for _, present := range []bool{false, true} {
		name := "missing-7233"
		if present {
			name = "existing-7233"
		}
		b.Run(name, func(b *testing.B) {
			// Small physical data, same file/piece count as the observed torrent.
			const pieces = 7233
			const pieceLength = 64
			data := bytes.Repeat([]byte{1}, pieceLength)
			hash := sha1.Sum(data)
			info := metainfo.Info{Name: "bundle", PieceLength: pieceLength, Pieces: bytes.Repeat(hash[:], pieces)}
			for i := 0; i < 10; i++ {
				n := int64(723 * pieceLength)
				if i == 9 {
					n += 3 * pieceLength
				}
				info.Files = append(info.Files, metainfo.FileInfo{Length: n, Path: []string{string(rune('a' + i))}})
			}
			encoded, err := bencode.Marshal(info)
			if err != nil {
				b.Fatal(err)
			}
			var events, readBytes, attempts int64
			started := time.Now()
			for n := 0; n < b.N; n++ {
				dir := b.TempDir()
				if present {
					if err := os.Mkdir(filepath.Join(dir, "bundle"), 0700); err != nil {
						b.Fatal(err)
					}
					for _, f := range info.Files {
						if err := os.WriteFile(filepath.Join(dir, "bundle", f.Path[0]), bytes.Repeat([]byte{1}, int(f.Length)), 0600); err != nil {
							b.Fatal(err)
						}
					}
				}
				root, err := os.OpenRoot(dir)
				if err != nil {
					b.Fatal(err)
				}
				var eventCount, byteCount atomic.Int64
				backend := storage.NewFileOpts(storage.NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, UsePartFiles: g.Some(false), NoCompletionRename: true, PieceCompletion: storage.NewMapPieceCompletion(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), IOEvent: func(string, string) { eventCount.Add(1) }, IORead: func(_ string, n int64) { byteCount.Add(n) }})
				cfg := TestingConfig(b)
				cfg.DefaultStorage = backend
				cfg.DisableTCP, cfg.DisableUTP, cfg.DisableWebtorrent = true, true, true
				cl, err := NewClient(cfg)
				if err != nil {
					b.Fatal(err)
				}
				spec := TorrentSpecFromMetaInfo(&metainfo.MetaInfo{InfoBytes: encoded})
				spec.DisableNetworking, spec.DisallowDataDownload = true, true
				tor, _, err := cl.AddTorrentSpec(spec)
				if err != nil {
					b.Fatal(err)
				}
				waitVerificationIdle(b, tor)
				cl.rLock()
				for i := range tor.pieces {
					attempts += int64(tor.pieces[i].numVerifies)
				}
				cl.rUnlock()
				events += eventCount.Load()
				readBytes += byteCount.Load()
				cl.Close()
				backend.Close()
			}
			b.ReportMetric(float64(time.Since(started).Microseconds())/float64(b.N), "startup-us/op")
			b.ReportMetric(float64(events)/float64(b.N), "fs-events/op")
			b.ReportMetric(float64(readBytes)/float64(b.N), "hash-bytes/op")
			b.ReportMetric(float64(attempts)/float64(b.N), "hash-attempts/op")
		})
	}
}
