package repository

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"torrent-stream-hub/internal/fastresume"
	"torrent-stream-hub/internal/models"
)

// The second v1 piece spans two files. The empty file and nested directory also
// require namespace durability. This complements the v1/v2/hybrid runtime matrix.
func openResumeBoundaryFixture(t *testing.T, dir string, restore bool, event func(string, string)) (*TorrentRepo, *StorageMapping, ResumeSource, *torrent.Torrent, *atomic.Int64) {
	t.Helper()
	data := bytes.Repeat([]byte("good"), 8192)
	info := &metainfo.Info{Name: "film", PieceLength: 16384, Files: []metainfo.FileInfo{
		{Path: []string{"a"}, Length: 20000}, {Path: []string{"nested", "b"}, Length: 12768}, {Path: []string{"empty"}, Length: 0},
	}}
	for off := 0; off < len(data); off += 16384 {
		h := sha1.Sum(data[off : off+16384])
		info.Pieces = append(info.Pieces, h[:]...)
	}
	encoded, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: encoded}
	db, err := NewSQLiteDB(filepath.Join(dir, "resume.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r := NewTorrentRepo(db)
	hash := mi.HashInfoBytes().HexString()
	if !restore {
		if err := r.SaveTorrent(&models.Torrent{Hash: hash, Name: "film", State: models.StatePaused}); err != nil {
			t.Fatal(err)
		}
		if err := r.EnsureStorageMapping(&StorageMapping{Hash: hash, Version: 2, Root: filepath.Join("payload", "film"), Prefix: "film", State: "ready"}); err != nil {
			t.Fatal(err)
		}
		offset := 0
		for _, f := range info.Files {
			path := filepath.Join(append([]string{dir, "payload", "film"}, f.Path...)...)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data[offset:offset+int(f.Length)], 0600); err != nil {
				t.Fatal(err)
			}
			offset += int(f.Length)
		}
	}
	m, err := r.StorageMapping(hash)
	if err != nil {
		t.Fatal(err)
	}
	i, err := r.GetIntent(hash)
	if err != nil {
		t.Fatal(err)
	}
	s := ResumeSource{Identity: fastresume.Identity{RecordID: i.Version.RecordID, StorageID: m.StorageID, MappingRevision: m.Revision, LayoutVersion: m.Version, Root: m.Root, Prefix: m.Prefix, VolumeID: "boundary-fixture"}, Metadata: mi, Info: info, Authorize: func(ctx context.Context) error { return ctx.Err() }}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	reads := &atomic.Int64{}
	opts := storage.NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, ReadOnly: restore, NoCompletionRename: true, UsePartFiles: g.Some(false), PieceCompletion: storage.NewMapPieceCompletion(), IOEvent: event, IORead: func(_ string, n int64) { reads.Add(n) }, TorrentPathResolver: func(base string, _ *metainfo.Info, _ metainfo.Hash) (string, []string, error) {
		return filepath.Join(base, m.Root), []string{"a", filepath.Join("nested", "b"), "empty"}, nil
	}}
	if !restore {
		opts.ResumeHooks = func(ctx context.Context, info *metainfo.Info, _ metainfo.Hash) (*storage.FileResumeHooks, error) {
			return r.NewResumeMutationHooks(ctx, m, s.Identity.RecordID, info.NumPieces(), info.PieceLength, false)
		}
	}
	backend := storage.NewFileOpts(opts)
	t.Cleanup(func() { _ = backend.Close() })
	wrapper := resumeLibraryBackend{ClientImpl: backend}
	if restore {
		wrapper.restore = func(ctx context.Context, _ *metainfo.Info, b storage.TorrentImpl) error {
			return r.RestoreResume(ctx, m, s, b)
		}
	}
	cfg := torrent.TestingConfig(t)
	cfg.DisableTCP, cfg.DisableUTP, cfg.DisableWebtorrent = true, true, true
	cfg.DefaultStorage = wrapper
	client, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		t.Fatal(err)
	}
	spec.DisableNetworking, spec.DisallowDataDownload, spec.DisallowDataUpload, spec.DisableInitialPieceCheck = true, true, true, restore
	tor, _, err := client.AddTorrentSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	s.Info, s.Capture = tor.Info(), tor.WithFileCheckpoint
	if !restore {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tor.VerifyDataContext(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return r, m, s, tor, reads
}

func TestResumeCrashSealBoundaries(t *testing.T) {
	if dir := os.Getenv("TSH_RESUME_BOUNDARY_DIR"); dir != "" {
		stage := os.Getenv("TSH_RESUME_BOUNDARY_STAGE")
		var armed atomic.Bool
		r, m, source, tor, _ := openResumeBoundaryFixture(t, dir, false, func(op, path string) {
			if armed.Load() && stage == op+":"+filepath.ToSlash(path) {
				resumeLibraryCrashBarrier()
			}
		})
		baseline := source
		baseline.Capture = func(ctx context.Context, publish func(storage.FileCheckpoint) error) error {
			return tor.WithFileCheckpoint(ctx, func(c storage.FileCheckpoint) error { c.Complete[0] = 1; return publish(c) })
		}
		if err := r.SealResume(context.Background(), m, baseline); err != nil {
			t.Fatal(err)
		}
		armed.Store(true)
		source.Capture = func(ctx context.Context, publish func(storage.FileCheckpoint) error) error {
			return tor.WithFileCheckpoint(ctx, func(c storage.FileCheckpoint) error {
				if stage == "commit-before" {
					resumeLibraryCrashBarrier()
				}
				if err := publish(c); err != nil {
					return err
				}
				if stage == "commit-after" {
					resumeLibraryCrashBarrier()
				}
				return nil
			})
		}
		worker, err := r.NewResumeProducer(func(context.Context, string) (*StorageMapping, ResumeSource, func(), error) {
			return m, source, func() {}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if !worker.MarkDirty(m.StorageID) {
			t.Fatal("owner not queued")
		}
		if os.Getenv("TSH_RESUME_BOUNDARY_PERIODIC") == "1" {
			// Actual production 60-second timer, no manual or shutdown trigger.
			<-worker.Done()
		} else if err := worker.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Fatal("crash boundary not reached")
	}
	stages := []string{"checkpoint-snapshot-before:", "checkpoint-snapshot-after:", "commit-before", "commit-after"}
	for _, path := range []string{"payload/film/a", "payload/film/nested/b", "payload/film/empty"} {
		for _, when := range []string{"before", "after"} {
			stages = append(stages, "checkpoint-payload-sync-"+when+":"+path)
		}
	}
	for _, path := range []string{".", "payload", "payload/film", "payload/film/nested"} {
		for _, when := range []string{"before", "after"} {
			stages = append(stages, "checkpoint-directory-sync-"+when+":"+path)
		}
	}
	for _, stage := range stages {
		t.Run("shutdown/"+stage, func(t *testing.T) { runResumeBoundaryCrash(t, stage, false) })
	}
	for _, stage := range []string{"commit-before", "commit-after"} {
		t.Run("periodic/"+stage, func(t *testing.T) {
			t.Parallel() // Both real ticker cases share the same wall-clock minute.
			runResumeBoundaryCrash(t, stage, true)
		})
	}
}

func runResumeBoundaryCrash(t *testing.T, stage string, periodic bool) {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestResumeCrashSealBoundaries$")
	cmd.Env = append(os.Environ(), "TSH_RESUME_BOUNDARY_DIR="+dir, "TSH_RESUME_BOUNDARY_STAGE="+stage)
	if periodic {
		cmd.Env = append(cmd.Env, "TSH_RESUME_BOUNDARY_PERIODIC=1")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("boundary %s: %q %v %s", stage, line, err, stderr.String())
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waited = true
	r, m, s, tor, reads := openResumeBoundaryFixture(t, dir, true, nil)
	saved, err := r.LoadResume(context.Background(), m, s.Identity.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	wantMask, generation := byte(1), int64(1)
	if stage == "commit-after" {
		wantMask, generation = 3, 2
	}
	if saved == nil || saved.Invalidated || saved.Token.Generation != generation || !bytes.Equal(saved.Checkpoint.Pieces, []byte{wantMask}) {
		t.Fatalf("partial seal escaped: %+v", saved)
	}
	assertResumeLibraryMask(t, tor, reads, wantMask)
	if tor.Files()[2].Length() != 0 {
		t.Fatal("empty file geometry changed")
	}
	// Boundary data on both sides of the shared piece is physically unchanged.
	for name, length := range map[string]int{"a": 20000, "nested/b": 12768} {
		data, err := os.ReadFile(filepath.Join(dir, "payload", "film", filepath.FromSlash(name)))
		if err != nil || len(data) != length || !bytes.Equal(data, bytes.Repeat([]byte("good"), length/4)) {
			t.Fatal("payload mutated", name, err)
		}
	}
	if wantMask == 3 {
		reader := tor.NewReader()
		defer reader.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		reader.SetContext(ctx)
		if _, err := reader.Seek(19998, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		var out [4]byte
		if _, err := io.ReadFull(reader, out[:]); err != nil || string(out[:]) != "odgo" {
			t.Fatal(fmt.Sprint(out), err)
		}
	}
}
