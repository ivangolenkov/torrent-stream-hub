package repository

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
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
	"github.com/anacrolix/torrent/merkle"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"torrent-stream-hub/internal/fastresume"
	"torrent-stream-hub/internal/models"
)

type resumeLibraryBackend struct {
	storage.ClientImpl
	restore func(context.Context, *metainfo.Info, storage.TorrentImpl) error
	opened  *storage.TorrentImpl
}

func (b resumeLibraryBackend) OpenTorrent(ctx context.Context, info *metainfo.Info, hash metainfo.Hash) (storage.TorrentImpl, error) {
	native, err := b.ClientImpl.OpenTorrent(ctx, info, hash)
	if err == nil && b.restore != nil {
		err = b.restore(ctx, info, native)
		if err != nil {
			_ = native.Close()
		}
	}
	if err == nil && b.opened != nil {
		*b.opened = native
	}
	return native, err
}

func openResumeLibrary(t *testing.T, dir, version string, restore bool, writable ...*storage.TorrentImpl) (*TorrentRepo, *StorageMapping, ResumeSource, *torrent.Torrent, *atomic.Int64) {
	t.Helper()
	ctx := context.Background()
	data := bytes.Repeat([]byte("good"), 8192)
	info := &metainfo.Info{Name: "film", PieceLength: 16384, Files: []metainfo.FileInfo{{Path: []string{"video"}, Length: int64(len(data))}}}
	mi := &metainfo.MetaInfo{}
	if version != "v2" {
		for off := 0; off < len(data); off += 16384 {
			h := sha1.Sum(data[off : off+16384])
			info.Pieces = append(info.Pieces, h[:]...)
		}
	}
	if version != "v1" {
		info.MetaVersion = 2
		if version == "v2" {
			info.Files = nil
		}
		h := merkle.NewHash()
		h.Write(data)
		root := string(h.Sum(nil))
		info.FileTree = metainfo.FileTree{Dir: map[string]metainfo.FileTree{"video": {File: metainfo.FileTreeFile{Length: int64(len(data)), PiecesRoot: root}}}}
		h = merkle.NewHash()
		h.Write(data[:16384])
		leaf := string(h.Sum(nil))
		mi.PieceLayers = map[string]string{root: leaf + leaf}
	}
	var err error
	mi.InfoBytes, err = bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	db, err := NewSQLiteDB(filepath.Join(dir, "resume.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := NewTorrentRepo(db)
	hash := mi.HashInfoBytes().HexString() // canonical app identity; v2 backend uses its SHA-256 short hash
	if !restore {
		if err := repo.SaveTorrent(&models.Torrent{Hash: hash, Name: "film", State: models.StatePaused}); err != nil {
			t.Fatal(err)
		}
		if err := repo.EnsureStorageMapping(&StorageMapping{Hash: hash, Version: 2, Root: filepath.Join("payload", "film"), Prefix: "film", State: "ready"}); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "payload", "film", "video")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	m, err := repo.StorageMapping(hash)
	if err != nil || m == nil {
		t.Fatal(m, err)
	}
	intent, err := repo.GetIntent(hash)
	if err != nil {
		t.Fatal(err)
	}
	source := ResumeSource{Identity: fastresume.Identity{RecordID: intent.Version.RecordID, StorageID: m.StorageID, MappingRevision: m.Revision, LayoutVersion: m.Version, Root: m.Root, Prefix: m.Prefix, VolumeID: "library-fixture"}, Metadata: mi, Info: info, Authorize: func(ctx context.Context) error { return ctx.Err() }}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	reads := &atomic.Int64{}
	opts := storage.NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, ReadOnly: restore && len(writable) == 0, NoCompletionRename: true, UsePartFiles: g.Some(false), PieceCompletion: storage.NewMapPieceCompletion(), IORead: func(_ string, n int64) { reads.Add(n) }, TorrentPathResolver: func(base string, _ *metainfo.Info, _ metainfo.Hash) (string, []string, error) {
		return filepath.Join(base, m.Root), []string{"video"}, nil
	}}
	if len(writable) != 0 {
		opts.ResumeHooks = func(ctx context.Context, info *metainfo.Info, _ metainfo.Hash) (*storage.FileResumeHooks, error) {
			hooks, err := repo.NewResumeMutationHooks(ctx, m, source.Identity.RecordID, info.NumPieces(), info.PieceLength, false)
			if err == nil && hooks != nil && os.Getenv("TSH_RESUME_LIBRARY_STAGE") == "before-overwrite" {
				revoke := hooks.Revoke
				hooks.Revoke = func(mask []byte) (bool, error) {
					discarded, err := revoke(mask)
					if err == nil {
						resumeLibraryCrashBarrier()
					}
					return discarded, err
				}
			}
			return hooks, err
		}
	}
	backend := storage.NewFileOpts(opts)
	t.Cleanup(func() { _ = backend.Close() })
	wrapper := resumeLibraryBackend{ClientImpl: backend}
	if len(writable) != 0 {
		wrapper.opened = writable[0]
	}
	if restore {
		wrapper.restore = func(ctx context.Context, validated *metainfo.Info, native storage.TorrentImpl) error {
			s := source
			s.Info = validated
			return repo.RestoreResume(ctx, m, s, native)
		}
	}
	cfg := torrent.TestingConfig(t)
	cfg.DisableTCP, cfg.DisableUTP, cfg.DisableWebtorrent = true, true, true
	cfg.DefaultStorage = wrapper
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		t.Fatal(err)
	}
	spec.DisableNetworking, spec.DisallowDataDownload, spec.DisallowDataUpload = true, true, true
	spec.DisableInitialPieceCheck = restore // revoked remainder must not gain startup readiness
	tor, _, err := cl.AddTorrentSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	source.Info = tor.Info()
	source.Capture = tor.WithFileCheckpoint
	if !restore {
		verify, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := tor.VerifyDataContext(verify); err != nil {
			t.Fatal(err)
		}
	}
	return repo, m, source, tor, reads
}

func assertResumeLibraryRange(t *testing.T, tor *torrent.Torrent, reads *atomic.Int64, revoked bool) {
	t.Helper()
	want := byte(3)
	if revoked {
		want = 1
	}
	assertResumeLibraryMask(t, tor, reads, want)
}

func assertResumeLibraryMask(t *testing.T, tor *torrent.Torrent, reads *atomic.Int64, want byte) {
	t.Helper()
	if reads.Load() != 0 || tor.PieceState(0).Complete != (want&1 != 0) || tor.PieceState(1).Complete != (want&2 != 0) {
		t.Fatal("restore hashed payload or granted wrong readiness", reads.Load())
	}
	if want == 0 {
		return
	}
	r := tor.NewReader()
	defer r.Close()
	r.SetReadahead(0)
	if want&1 == 0 {
		if _, err := r.Seek(16384, io.SeekStart); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	buf := make([]byte, 16)
	if n, err := r.ReadContext(ctx, buf); err != nil || n != len(buf) || string(buf) != "goodgoodgoodgood" {
		t.Fatal(n, err, string(buf))
	}
}

func resumeLibraryCrashBarrier() {
	_, _ = os.Stdout.WriteString("ready\n")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestResumeAdapterLibraryClean(t *testing.T) {
	for _, version := range []string{"v1", "v2", "hybrid"} {
		t.Run(version, func(t *testing.T) {
			dir := t.TempDir()
			t.Run("seal", func(t *testing.T) {
				r, m, source, _, _ := openResumeLibrary(t, dir, version, false)
				worker, err := r.NewResumeProducer(func(context.Context, string) (*StorageMapping, ResumeSource, func(), error) {
					return m, source, func() {}, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				worker.MarkDirty(m.StorageID)
				if err := worker.Shutdown(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := worker.Wait(); err != nil {
					t.Fatal(err)
				}
			})
			for i := 0; i < 2; i++ {
				t.Run("reopen", func(t *testing.T) {
					_, _, _, tor, reads := openResumeLibrary(t, dir, version, true)
					assertResumeLibraryRange(t, tor, reads, false)
				})
			}
		})
	}
}

func TestResumeAdapterLibraryCrash(t *testing.T) {
	if dir := os.Getenv("TSH_RESUME_LIBRARY_DIR"); dir != "" {
		version, stage := os.Getenv("TSH_RESUME_LIBRARY_VERSION"), os.Getenv("TSH_RESUME_LIBRARY_STAGE")
		var native storage.TorrentImpl
		var writable []*storage.TorrentImpl
		if stage == "before-overwrite" || stage == "after-overwrite" {
			writable = append(writable, &native)
		}
		r, m, source, tor, _ := openResumeLibrary(t, dir, version, false, writable...)
		if err := r.SealResume(context.Background(), m, source); err != nil {
			t.Fatal(err)
		}
		barrier := resumeLibraryCrashBarrier
		switch stage {
		case "before-commit":
			source.Capture = func(ctx context.Context, publish func(storage.FileCheckpoint) error) error {
				return tor.WithFileCheckpoint(ctx, func(c storage.FileCheckpoint) error { barrier(); return publish(c) })
			}
			if err := r.SealResume(context.Background(), m, source); err != nil {
				t.Fatal(err)
			}
		case "after-revoke":
			s, err := r.LoadResume(context.Background(), m, source.Identity.RecordID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.RevokeResume(context.Background(), m, s.Token, []byte{2}); err != nil {
				t.Fatal(err)
			}
			barrier()
		case "before-overwrite", "after-overwrite":
			if n, err := native.Piece(source.Info.Piece(0)).WriteAt([]byte("evil"), 0); err != nil || n != 4 {
				t.Fatal(n, err)
			}
			barrier()
		default:
			barrier()
		}
		return
	}
	for _, version := range []string{"v1", "v2", "hybrid"} {
		for _, stage := range []string{"after-seal", "before-commit", "after-revoke", "before-overwrite", "after-overwrite"} {
			t.Run(version+"/"+stage, func(t *testing.T) {
				dir := t.TempDir()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestResumeAdapterLibraryCrash$")
				cmd.Env = append(os.Environ(), "TSH_RESUME_LIBRARY_DIR="+dir, "TSH_RESUME_LIBRARY_VERSION="+version, "TSH_RESUME_LIBRARY_STAGE="+stage)
				stdin, err := cmd.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				defer stdin.Close()
				stdout, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				cmd.Stderr = os.Stderr
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
					t.Fatal("child barrier", line, err)
				}
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				waited = true
				restored, mapping, binding, tor, reads := openResumeLibrary(t, dir, version, true)
				switch stage {
				case "before-overwrite":
					assertResumeLibraryMask(t, tor, reads, 2)
				case "after-overwrite":
					saved, err := restored.LoadResume(context.Background(), mapping, binding.Identity.RecordID)
					if err != nil {
						t.Fatal(err)
					}
					st, err := os.Stat(filepath.Join(dir, "payload", "film", "video"))
					if err != nil {
						t.Fatal(err)
					}
					want := byte(0)
					if st.ModTime().UnixNano() == saved.Checkpoint.Files[0].Evidence.ModifiedNS && st.Size() == saved.Checkpoint.Files[0].Evidence.Size {
						want = 2
					}
					assertResumeLibraryMask(t, tor, reads, want)
				default:
					assertResumeLibraryRange(t, tor, reads, stage == "after-revoke")
				}
				data, err := os.ReadFile(filepath.Join(dir, "payload", "film", "video"))
				if err != nil {
					t.Fatal(err)
				}
				prefix := "good"
				if stage == "after-overwrite" {
					prefix = "evil"
				}
				if string(data[:4]) != prefix {
					t.Fatal("write crossed durable crash barrier", string(data[:4]))
				}
			})
		}
	}
}
