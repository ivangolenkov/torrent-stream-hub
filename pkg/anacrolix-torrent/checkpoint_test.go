package torrent

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/merkle"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

func checkpointMetainfo(t *testing.T, mode, name string, data []byte) *metainfo.MetaInfo {
	t.Helper()
	info := metainfo.Info{Name: name, PieceLength: 16384, Files: []metainfo.FileInfo{{Path: []string{"video.bin"}, Length: int64(len(data))}}}
	if mode != "v2" {
		for off := 0; off < len(data); off += 16384 {
			h := sha1.Sum(data[off:min(off+16384, len(data))])
			info.Pieces = append(info.Pieces, h[:]...)
		}
	}
	var layers map[string]string
	if mode == "v2" || mode == "hybrid" {
		info.MetaVersion = 2
		if mode == "v2" {
			info.Files = nil
		}
		h := merkle.NewHash()
		h.Write(data)
		root := string(h.Sum(nil))
		info.FileTree = metainfo.FileTree{Dir: map[string]metainfo.FileTree{"video.bin": {File: metainfo.FileTreeFile{Length: int64(len(data)), PiecesRoot: root}}}}
		var layer []byte
		for off := 0; off < len(data); off += 16384 {
			h := merkle.NewHash()
			h.Write(data[off:min(off+16384, len(data))])
			layer = h.SumMinLength(layer, 16384)
		}
		if len(data) > 16384 {
			layers = map[string]string{root: string(layer)}
		}
	}
	raw, err := bencode.Marshal(&info)
	if err != nil {
		t.Fatal(err)
	}
	return &metainfo.MetaInfo{InfoBytes: raw, PieceLayers: layers}
}

type checkpointRestoreBackend struct {
	storage.ClientImpl
	claims *storage.FileCheckpoint
}

func (b checkpointRestoreBackend) OpenTorrent(ctx context.Context, info *metainfo.Info, hash metainfo.Hash) (storage.TorrentImpl, error) {
	ts, err := b.ClientImpl.OpenTorrent(ctx, info, hash)
	if err != nil || b.claims == nil || b.claims.InfoHash != hash {
		return ts, err
	}
	token, err := ts.PrepareRestore(ctx, *b.claims, func(ctx context.Context) error { return ctx.Err() })
	if err == nil {
		err = ts.RestoreCheckpoint(ctx, token)
	}
	if err != nil {
		ts.Close()
	}
	return ts, err
}

func checkpointClient(t *testing.T, dir string, claims *storage.FileCheckpoint, hook func(string, int64), resume ...storage.FileResumeFactory) (*Client, *atomic.Int64) {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	reads := &atomic.Int64{}
	opts := storage.NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, ReadOnly: claims != nil, NoCompletionRename: true, UsePartFiles: g.Some(false), PieceCompletion: storage.NewMapPieceCompletion(),
		IORead: func(path string, n int64) {
			reads.Add(n)
			if hook != nil {
				hook(path, n)
			}
		},
		TorrentPathResolver: func(base string, info *metainfo.Info, _ metainfo.Hash) (string, []string, error) {
			var paths []string
			for _, f := range info.UpvertedFiles() {
				paths = append(paths, filepath.Join(f.Path...))
			}
			return filepath.Join(base, "payload", info.Name), paths, nil
		}}
	if len(resume) != 0 {
		opts.ResumeHooks = resume[0]
		opts.ReadOnly = false
	}
	backend := storage.NewFileOpts(opts)
	t.Cleanup(func() { _ = backend.Close() })
	cfg := TestingConfig(t)
	cfg.DisableTCP, cfg.DisableUTP, cfg.DisableWebtorrent = true, true, true
	cfg.DefaultStorage = checkpointRestoreBackend{backend, claims}
	cfg.PieceHashersPerTorrent, cfg.MaxActivePieceHashers = 1, 1
	cl, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	return cl, reads
}

func TestFileCheckpointChangedLayerRevokesBeforeApply(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "durability-failure"}[fail], func(t *testing.T) {
			dir, mi, _, claim := warmCheckpoint(t, "v2")
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			cl, _ := checkpointClient(t, dir, &claim, nil, func(context.Context, *metainfo.Info, metainfo.Hash) (*storage.FileResumeHooks, error) {
				return &storage.FileResumeHooks{Pending: []byte{3}, Revoke: func(mask []byte) (bool, error) {
					if !bytes.Equal(mask, []byte{1}) {
						t.Error("wrong layer revoke", mask)
					}
					close(entered)
					<-release
					if fail {
						return false, errors.New("durability failure")
					}
					return false, nil
				}, Invalidate: func() error { return nil }}, nil
			})
			t.Cleanup(unblock)
			tor := addCheckpointTorrent(t, cl, mi, nil)
			cl.lock()
			cl.config.PieceHashersPerTorrent = 0
			before := tor.pieces[0].hashV2.Value
			after := before
			after[0] ^= 1
			cl.unlock()
			done := make(chan struct{})
			go func() { cl.lock(); tor.pieces[0].setV2Hash(after); cl.unlock(); close(done) }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("layer mutation omitted durable revoke")
			}
			observed := make(chan bool, 1)
			go func() { observed <- tor.PieceState(0).Complete }()
			select {
			case complete := <-observed:
				if complete {
					t.Fatal("known-invalid layer remained live during blocked revoke")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("layer revoke held Client.lock")
			}
			unblock()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("layer mutation did not join")
			}
			cl.rLock()
			got := tor.pieces[0].hashV2.Value
			cl.rUnlock()
			want := after
			if fail {
				want = before
			}
			if got != want || tor.PieceState(0).Complete || !tor.PieceState(1).Complete {
				t.Fatal("layer mutation bypassed fencing or revoked independent piece")
			}
		})
	}
}

func TestFileCheckpointEmptyReadinessRequiresRootedProof(t *testing.T) {
	for _, writable := range []bool{false, true} {
		t.Run(map[bool]string{false: "read-only", true: "writable"}[writable], func(t *testing.T) {
			dir := t.TempDir()
			base := filepath.Join(dir, "payload", "film")
			if err := os.MkdirAll(base, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(base, "a"), []byte("data"), 0600); err != nil {
				t.Fatal(err)
			}
			hash := sha1.Sum([]byte("data"))
			info := &metainfo.Info{Name: "film", PieceLength: 4, Pieces: hash[:], Files: []metainfo.FileInfo{{Path: []string{"a"}, Length: 4}, {Path: []string{"empty"}, Length: 0}}}
			raw, err := bencode.Marshal(info)
			if err != nil {
				t.Fatal(err)
			}
			mi := &metainfo.MetaInfo{InfoBytes: raw}
			warm, _ := checkpointClient(t, dir, nil, nil)
			tor := addCheckpointTorrent(t, warm, mi, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := tor.VerifyDataContext(ctx); err != nil {
				t.Fatal(err)
			}
			var claim storage.FileCheckpoint
			if err := tor.WithFileCheckpoint(ctx, func(c storage.FileCheckpoint) error { claim = c; return nil }); err != nil {
				t.Fatal(err)
			}
			warm.Close()
			if err := os.Remove(filepath.Join(base, "empty")); err != nil {
				t.Fatal(err)
			}
			var factories []storage.FileResumeFactory
			if writable {
				factories = append(factories, func(context.Context, *metainfo.Info, metainfo.Hash) (*storage.FileResumeHooks, error) {
					return &storage.FileResumeHooks{Pending: []byte{1}, RestoreCandidate: true, Revoke: func([]byte) (bool, error) { return false, nil }, Invalidate: func() error { t.Error("restore created a missing empty file"); return nil }}, nil
				})
			}
			cold, reads := checkpointClient(t, dir, &claim, nil, factories...)
			restored := addCheckpointTorrent(t, cold, mi, nil)
			files := restored.Files()
			if reads.Load() != 0 || !files[0].ReadyForDirectRead() || files[1].CachedReadyForDirectRead() || files[1].ReadyForDirectRead() || restored.FileObservations()[1].DirectReadReady {
				t.Fatal("empty readiness was inferred without rooted proof")
			}
			if _, err := os.Stat(filepath.Join(base, "empty")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("restore manufactured empty payload", err)
			}
		})
	}
}

func addCheckpointTorrent(t *testing.T, cl *Client, mi *metainfo.MetaInfo, edit func(*TorrentSpec)) *Torrent {
	t.Helper()
	spec, err := TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		t.Fatal(err)
	}
	spec.DisableNetworking, spec.DisallowDataDownload, spec.DisallowDataUpload = true, true, true
	if edit != nil {
		edit(spec)
	}
	tor, _, err := cl.AddTorrentSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	return tor
}

func warmCheckpoint(t *testing.T, mode string) (string, *metainfo.MetaInfo, []byte, storage.FileCheckpoint) {
	t.Helper()
	dir := t.TempDir()
	data := bytes.Repeat([]byte("good"), 8192)
	path := filepath.Join(dir, "payload", "film", "video.bin")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	mi := checkpointMetainfo(t, mode, "film", data)
	cl, _ := checkpointClient(t, dir, nil, nil)
	tor := addCheckpointTorrent(t, cl, mi, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tor.VerifyDataContext(ctx); err != nil {
		t.Fatal(err)
	}
	var c storage.FileCheckpoint
	if err := tor.WithFileCheckpoint(ctx, func(v storage.FileCheckpoint) error { c = v; return nil }); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(c.Complete, []byte{3}) {
		t.Fatal("incomplete warm fixture", c.Complete)
	}
	cl.Close()
	return dir, mi, data, c
}

func TestFileCheckpointColdRestoreVersions(t *testing.T) {
	for _, mode := range []string{"v1", "v2", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			dir, mi, data, c := warmCheckpoint(t, mode)
			cl, reads := checkpointClient(t, dir, &c, nil)
			tor := addCheckpointTorrent(t, cl, mi, nil)
			if reads.Load() != 0 {
				t.Fatal("cold restore rehashed", reads.Load())
			}
			cl.rLock()
			for i := range tor.pieces {
				if !tor.pieceComplete(i) || tor.pieces[i].numVerifies != 0 {
					cl.rUnlock()
					t.Fatal("restore was not complete without hashing", i)
				}
			}
			cl.rUnlock()
			if !tor.Files()[0].ReadyForDirectRead() {
				t.Fatal("restored file not ready")
			}
			r := tor.NewReader()
			defer r.Close()
			r.SetReadahead(0)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			out := make([]byte, 16)
			n, err := r.ReadContext(ctx, out)
			if err != nil && err != io.EOF || n != len(out) || !bytes.Equal(out, data[:16]) {
				t.Fatal(n, err, out)
			}
		})
	}
}

func TestFileCheckpointPartialReaderAndV2Layers(t *testing.T) {
	for _, mode := range []string{"v1", "v2", "hybrid"} {
		t.Run("partial-"+mode, func(t *testing.T) {
			dir, mi, data, c := warmCheckpoint(t, mode)
			c.Complete[0] = 1
			entered, release := make(chan struct{}), make(chan struct{})
			var first atomic.Bool
			cl, _ := checkpointClient(t, dir, &c, func(string, int64) {
				if first.CompareAndSwap(false, true) {
					close(entered)
					<-release
				}
			})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			t.Cleanup(unblock)
			// Finish initial layer installation before blocking the remainder
			// hasher: hybrid MergeSpec legitimately joins v1 hashing when
			// installing v2 hashes under storageLock.
			cl.lock()
			cl.config.PieceHashersPerTorrent = 0
			cl.unlock()
			tor := addCheckpointTorrent(t, cl, mi, nil)
			cl.lock()
			cl.config.PieceHashersPerTorrent = 1
			cl.startPieceHashers()
			cl.unlock()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("untrusted remainder was not hashed")
			}
			if !tor.PieceState(0).Complete || tor.PieceState(1).Complete {
				t.Fatal("partial mask expanded")
			}
			r := tor.NewReader()
			defer r.Close()
			r.SetReadahead(0)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			out := make([]byte, 16)
			n, err := r.ReadContext(ctx, out)
			if err != nil || n != 16 || !bytes.Equal(out, data[:16]) {
				t.Fatal("trusted range waited for unrelated hash", n, err)
			}
			unblock()
			select {
			case <-tor.Complete().On():
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
	t.Run("missing-v2-layers", func(t *testing.T) {
		dir, mi, _, c := warmCheckpoint(t, "v2")
		cl, reads := checkpointClient(t, dir, &c, nil)
		tor := addCheckpointTorrent(t, cl, mi, func(s *TorrentSpec) { s.PieceLayers = nil })
		if tor.PieceState(0).Complete || tor.Files()[0].CachedReadyForDirectRead() || reads.Load() != 0 {
			t.Fatal("unknown v2 layers granted readiness")
		}
		addCheckpointTorrent(t, cl, mi, nil)
		if !tor.PieceState(0).Complete || !tor.Files()[0].ReadyForDirectRead() || reads.Load() != 0 {
			t.Fatal("valid v2 layers not applied without rehash", reads.Load())
		}
	})
	t.Run("ignore-unverified", func(t *testing.T) {
		dir, mi, _, c := warmCheckpoint(t, "v1")
		cl, _ := checkpointClient(t, dir, &c, nil)
		tor := addCheckpointTorrent(t, cl, mi, func(s *TorrentSpec) { s.IgnoreUnverifiedPieceCompletion = true; s.DisableInitialPieceCheck = true })
		if tor.PieceState(0).Complete || tor.Files()[0].CachedReadyForDirectRead() {
			t.Fatal("checkpoint bypassed forced-verification policy")
		}
	})
}

func TestFileCheckpointQuiesceReadsAndDropJoin(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte("good"), 8192)
	path := filepath.Join(dir, "payload", "film", "video.bin")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cl, _ := checkpointClient(t, dir, nil, nil)
	tor := addCheckpointTorrent(t, cl, checkpointMetainfo(t, "v1", "film", data), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tor.VerifyDataContext(ctx); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	t.Cleanup(unblock)
	job := make(chan error, 1)
	go func() {
		job <- tor.WithFileCheckpoint(ctx, func(storage.FileCheckpoint) error { close(entered); <-release; return nil })
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Both another torrent and an existing reader must progress during publish.
	other := make(chan *Torrent, 1)
	otherSpec := TorrentSpecFromMetaInfo(checkpointMetainfo(t, "v1", "other", data))
	otherSpec.DisableNetworking, otherSpec.DisallowDataDownload = true, true
	go func() { v, _, _ := cl.AddTorrentSpec(otherSpec); other <- v }()
	select {
	case v := <-other:
		if v == nil {
			t.Fatal("unrelated Add failed")
		}
	case <-ctx.Done():
		t.Fatal("checkpoint held Client.lock")
	}
	r := tor.NewReader()
	defer r.Close()
	r.SetReadahead(0)
	out := make([]byte, 16)
	if n, err := r.ReadContext(ctx, out); err != nil || n != 16 || !bytes.Equal(out, data[:16]) {
		t.Fatal("checkpoint blocked reader", n, err)
	}
	dropped := make(chan struct{})
	go func() { tor.Drop(); close(dropped) }()
	select {
	case <-tor.closed.Done():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-dropped:
		t.Fatal("Drop returned before checkpoint callback joined")
	default:
	}
	unblock()
	select {
	case <-dropped:
	case <-ctx.Done():
		t.Fatal("Drop did not finish")
	}
	if err := <-job; err == nil {
		t.Fatal("closed checkpoint job reported success")
	}
}
