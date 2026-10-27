package usecase

import (
	"context"
	"crypto/sha1"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

func TestManualDeleteDoesNotGuessSecondPath(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor := temporaryFile(t, uc, "movie", true)
	if err := uc.MakePermanent(tor.Hash); err != nil {
		t.Fatal(err)
	}
	paths, err := uc.knownStoragePaths(tor)
	if err != nil {
		t.Fatal(err)
	}
	root := uc.engine.DownloadDir()
	for _, name := range []string{paths[0], paths[0] + ".part", "nested/video.bin", "movie/keep.txt"} {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("keep or delete exactly"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := uc.Delete(tor.Hash, true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{paths[0], paths[0] + ".part"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("payload survived: %s %v", name, err)
		}
	}
	for _, name := range []string{"nested/video.bin", "movie/keep.txt"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("foreign file lost: %s %v", name, err)
		}
	}
	if got, _ := repo.GetTorrent(tor.Hash); got != nil {
		t.Fatal("record survived deletion")
	}
}

func TestManualDeleteUnknownLayoutNeverRecurses(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	if err := repo.SaveTorrent(&models.Torrent{Hash: testInfoHash, Name: "foreign", State: models.StatePaused}); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(uc.engine.DownloadDir(), "foreign", "keep")
	if err := os.MkdirAll(filepath.Dir(foreign), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := uc.Delete(testInfoHash, true); !errors.Is(err, ErrStorageConflict) {
		t.Fatalf("unknown layout: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatal(err)
	}
	if err := uc.Delete(testInfoHash, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatal(err)
	}
}

func TestManualDeleteLeaseAndAdmissionAreSerialized(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor := temporaryFile(t, uc, "lease", false)
	release, err := uc.AcquireTransfer(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := uc.Delete(tor.Hash, true); !errors.Is(err, ErrStorageConflict) {
		t.Fatal(err)
	}
	if _, pending, _ := repo.CleanupPaths(tor.Hash); pending {
		t.Fatal("protected deletion modified intent")
	}
	release()
	if err := uc.Delete(tor.Hash, false); !errors.Is(err, ErrStorageConflict) {
		t.Fatal("debounce ignored")
	}
	uc.protectedUntil[tor.Hash] = time.Time{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	go func() { defer wg.Done(); <-start; _ = uc.Delete(tor.Hash, true) }()
	go func() {
		defer wg.Done()
		<-start
		if release, err := uc.AcquireTransfer(tor.Hash); err == nil {
			release()
		}
	}()
	close(start)
	wg.Wait()
	// Either admission won (record stays protected), or deletion won (no new lease).
	if got, _ := repo.GetTorrent(tor.Hash); got == nil && uc.transfers[tor.Hash] != 0 {
		t.Fatal("deleted with an active lease")
	}
}

func TestManualDeletionIntentSurvivesRestartAndBlocksReadd(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DownloadDir: dir, DBPath: filepath.Join(dir, "hub.db"), TorrentPort: 0}
	db, err := repository.NewSQLiteDB(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(cfg)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	repo := repository.NewTorrentRepo(db)
	uc := NewTorrentUseCase(eng, repo)
	tor, _ := completedSafetyFile(t, uc, "manual-retry", true)
	if err := uc.MakePermanent(tor.Hash); err != nil {
		t.Fatal(err)
	}
	paths, err := uc.knownStoragePaths(tor)
	if err != nil {
		t.Fatal(err)
	}
	// Fail finalizing the manifest: no unlink is allowed before durable commit.
	full := filepath.Join(dir, paths[0])
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("keep until manifest is durable"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`CREATE TRIGGER fail_manual_manifest BEFORE UPDATE OF cleanup_ready ON torrents WHEN NEW.cleanup_ready = 1 BEGIN SELECT RAISE(ABORT, 'injected DB failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := uc.Delete(tor.Hash, true); err == nil {
		t.Fatal("manifest error ignored")
	}
	if _, err := os.Stat(full); err != nil {
		t.Fatal("unlinked before manifest commit")
	}
	if _, err := db.DB().Exec(`DROP TRIGGER fail_manual_manifest`); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(full); err != nil {
		t.Fatal(err)
	}
	// A directory instead of payload must now fail validation, leaving a retryable intent.
	if err := os.Mkdir(full, 0700); err != nil {
		t.Fatal(err)
	}
	if err := uc.Delete(tor.Hash, true); !errors.Is(err, ErrStorageConflict) {
		t.Fatalf("delete: %v", err)
	}
	if err := uc.Resume(tor.Hash); !errors.Is(err, ErrStorageConflict) {
		t.Fatal(err)
	}
	if _, err := uc.AddMagnet("magnet:?xt=urn:btih:" + tor.Hash); !errors.Is(err, ErrStorageConflict) {
		t.Fatal(err)
	}
	if _, err := uc.AcquireTransfer(tor.Hash); !errors.Is(err, ErrStorageConflict) {
		t.Fatal(err)
	}
	eng.Close()
	db.Close()
	db, err = repository.NewSQLiteDB(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	eng, err = engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	uc = NewTorrentUseCase(eng, repository.NewTorrentRepo(db))
	if err := uc.RestoreTorrents(); err != nil {
		t.Fatal(err)
	}
	if eng.GetTorrent(tor.Hash) != nil {
		t.Fatal("pending deletion restored engine")
	}
	if err := os.Remove(full); err != nil {
		t.Fatal(err)
	}
	// Simulate already removed final file and a remaining part after interruption.
	if err := os.WriteFile(full+".part", []byte("remaining"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := uc.Delete(tor.Hash, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(full + ".part"); !os.IsNotExist(err) {
		t.Fatal("partial retry failed")
	}
}

func completedSafetyFile(t *testing.T, uc *TorrentUseCase, name string, multi bool) (*models.Torrent, string) {
	t.Helper()
	// DB-only legacy fixture: no background hasher may rename our test files.
	info := metainfo.Info{Name: name, Length: 1024, PieceLength: 1024, Pieces: make([]byte, 20)}
	digest := sha1.Sum(make([]byte, 1024))
	info.Pieces = digest[:]
	if multi {
		info.Length = 0
		info.Files = []metainfo.FileInfo{{Length: 1024, Path: []string{"nested", "video.bin"}}}
	}
	data, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: data}
	tor := &models.Torrent{Hash: mi.HashInfoBytes().HexString(), Name: name}
	metaDir := filepath.Join(uc.engine.ConfigDir(), "metainfo")
	if err := os.MkdirAll(metaDir, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(metaDir, tor.Hash+".torrent"))
	if err != nil {
		t.Fatal(err)
	}
	if err := mi.Write(f); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	paths, err := storagePaths(&info)
	if err != nil {
		t.Fatal(err)
	}
	if err := uc.repo.SaveStorageMapping(&repository.StorageMapping{Hash: tor.Hash, Version: 1, Root: filepath.Join("payload", tor.Hash), State: "ready"}); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(uc.engine.DownloadDir(), "payload", tor.Hash, paths[0])
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, make([]byte, 1024), 0600); err != nil {
		t.Fatal(err)
	}
	tor.Size, tor.Downloaded, tor.State = 1024, 1024, models.StateSeeding
	// Catalog paths use '/' on every platform; paths above are native disk
	// paths. Mixing them makes SaveCatalog reject this fixture on Windows.
	fi := info.UpvertedFiles()[0]
	tor.Files = []*models.File{{Index: 0, Path: fi.DisplayPath(&info), Size: 1024, Downloaded: 1024}}
	if err := uc.repo.SaveTorrent(tor); err != nil {
		t.Fatal(err)
	}
	if ready, err := uc.inspectInventory(context.Background(), tor.Hash, false); err != nil || !ready {
		t.Fatal("fixture inventory", ready, err)
	}
	raw, err := uc.engine.AddPausedMetainfo(&mi)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.VerifyData(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		ready, _, err := uc.engine.CheckFileDirectReady(tor.Hash, 0)
		if err != nil {
			t.Fatal(err)
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture never became verified")
		}
		time.Sleep(time.Millisecond)
	}
	return tor, full
}

func TestDownloadRevalidatesOnEveryOpen(t *testing.T) {
	for _, mutation := range []string{"symlink", "parent-symlink", "directory", "size", "part", "missing", "shared", "hardlink", "service-hardlink"} {
		t.Run(mutation, func(t *testing.T) {
			uc, _, cleanup := setupTorrentUseCase(t)
			defer cleanup()
			tor, full := completedSafetyFile(t, uc, "download", true)
			_, file, err := uc.GetDownloadedFile(tor.Hash, 0)
			if err != nil {
				t.Fatal(err)
			}
			external := filepath.Join(t.TempDir(), "external")
			if err := os.WriteFile(external, make([]byte, 1024), 0600); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "symlink":
				if err := os.Remove(full); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, full); err != nil {
					t.Skip(err)
				}
			case "parent-symlink":
				parent := filepath.Dir(full)
				if err := os.Rename(parent, parent+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(external), parent); err != nil {
					t.Skip(err)
				}
			case "directory":
				if err := os.Remove(full); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(full, 0700); err != nil {
					t.Fatal(err)
				}
			case "size":
				if err := os.Truncate(full, 1); err != nil {
					t.Fatal(err)
				}
			case "part":
				if err := os.WriteFile(full+".part", []byte("partial"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(full); err != nil {
					t.Fatal(err)
				}
			case "shared", "hardlink":
				other, otherFull := completedSafetyFile(t, uc, "other", true)
				if mutation == "shared" {
					paths, err := uc.knownStoragePaths(tor)
					if err != nil {
						t.Fatal(err)
					}
					if err := uc.repo.SaveCleanupPaths(other.Hash, paths); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Remove(otherFull); err != nil {
						t.Fatal(err)
					}
					if err := os.Link(full, otherFull); err != nil {
						t.Skip(err)
					}
				}
			case "service-hardlink":
				if err := os.Remove(full); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(filepath.Join(uc.engine.DownloadDir(), ".torrent.bolt.db"), full); err != nil {
					t.Skip(err)
				}
			}
			in, err := file.Open()
			if mutation == "part" {
				// Plain-name storage ignores an unrelated sibling added after
				// open; current-file verification still controls readiness.
				if err != nil {
					t.Fatal("verified metadata file blocked by sibling", err)
				}
				in.Close()
				if _, _, err := uc.GetDownloadedTorrentFiles(tor.Hash); err != nil {
					t.Fatal("verified ZIP payload blocked by sibling", err)
				}
				return
			}
			if err == nil {
				in.Close()
				t.Fatal("unsafe payload reopened after preflight")
			}
			if _, _, err := uc.GetDownloadedTorrentFiles(tor.Hash); err == nil {
				t.Fatal("unsafe ZIP manifest accepted")
			}
		})
	}
}
