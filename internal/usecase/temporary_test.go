package usecase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

func temporaryMagnet(t *testing.T, uc *TorrentUseCase, id int) *models.Torrent {
	t.Helper()
	torrent, err := uc.AddMagnetWithPolicy(fmt.Sprintf("magnet:?xt=urn:btih:%040x", id), TorrentMetadata{}, true)
	if err != nil {
		t.Fatal(err)
	}
	return torrent
}

func temporaryFile(t *testing.T, uc *TorrentUseCase, name string, multi bool) *models.Torrent {
	t.Helper()
	info := metainfo.Info{Name: name, PieceLength: 1024, Length: 1024, Pieces: make([]byte, 20)}
	if multi {
		info.Length = 0
		info.Files = []metainfo.FileInfo{{Length: 1024, Path: []string{"nested", "video.bin"}}}
	}
	data, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: data}
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		t.Fatal(err)
	}
	torrent, err := uc.AddTorrentFileWithPolicy(&buf, TorrentMetadata{Title: "Test"}, true)
	if err != nil {
		t.Fatal(err)
	}
	return torrent
}

func TestTemporaryFIFOAndPermanentPolicy(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	a := temporaryMagnet(t, uc, 1)
	b := temporaryMagnet(t, uc, 2)
	c := temporaryMagnet(t, uc, 3)
	// A repeat must not advance FIFO or trigger cleanup.
	temporaryMagnet(t, uc, 1)
	d := temporaryMagnet(t, uc, 4)
	if got, err := repo.GetTorrent(a.Hash); err != nil || got != nil {
		t.Fatalf("oldest survived: %v %v", got, err)
	}
	if uc.engine.GetTorrent(a.Hash) != nil {
		t.Fatal("deleted torrent survived in engine")
	}
	for i := 0; i < 2; i++ {
		if err := uc.MakePermanent(b.Hash); err != nil {
			t.Fatal(err)
		}
	}
	temporaryMagnet(t, uc, 2)
	if got, _ := repo.GetTorrent(b.Hash); got.IsTemporary {
		t.Fatal("permanent downgraded")
	}
	if err := uc.SetTemporaryLimit(1); err != nil {
		t.Fatal(err)
	}
	uc.CleanupTemporary("startup")
	q, err := repo.TemporaryQueue()
	if err != nil || !reflect.DeepEqual(q, []string{d.Hash}) {
		t.Fatalf("queue=%v err=%v (c=%s)", q, err, c.Hash)
	}
	if err := uc.MakePermanent("missing"); err == nil {
		t.Fatal("missing pin succeeded")
	}
}

func TestTemporaryTransferProtectionAndRetry(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	if err := uc.SetTemporaryLimit(1); err != nil {
		t.Fatal(err)
	}
	a := temporaryMagnet(t, uc, 1)
	release, err := uc.AcquireTransfer(a.Hash)
	if err != nil {
		t.Fatal(err)
	}
	temporaryMagnet(t, uc, 2)
	temporaryMagnet(t, uc, 3)
	q, _ := repo.TemporaryQueue()
	if len(q) != 3 {
		t.Fatal("active oldest must block FIFO, not evict newer torrents")
	}
	release()
	release()
	uc.CleanupTemporary("debounce")
	q, _ = repo.TemporaryQueue()
	if len(q) != 3 {
		t.Fatal("debounce was ignored")
	}
	uc.repo.Lifecycle.Lock()
	uc.protectedUntil[a.Hash] = time.Now().Add(-time.Second)
	uc.repo.Lifecycle.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); uc.runTemporaryCleanup(ctx, time.Millisecond) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		q, err = repo.TemporaryQueue()
		if err != nil {
			t.Fatal(err)
		}
		if len(q) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cleanup did not retry without new add")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := uc.AcquireTransfer(a.Hash); err == nil {
		t.Fatal("deleted torrent admitted a transfer")
	}
}

func TestTemporaryCleanupDeletesExactFiles(t *testing.T) {
	for _, multi := range []bool{false, true} {
		t.Run(fmt.Sprint(multi), func(t *testing.T) {
			uc, repo, cleanup := setupTorrentUseCase(t)
			defer cleanup()
			if err := uc.SetTemporaryLimit(10); err != nil {
				t.Fatal(err)
			}
			a := temporaryFile(t, uc, "old", multi)
			b := temporaryFile(t, uc, "new", false)
			if err := uc.MakePermanent(b.Hash); err != nil {
				t.Fatal(err)
			}
			paths, err := uc.knownStoragePaths(a)
			if err != nil || len(paths) != 1 {
				t.Fatalf("paths=%v %v", paths, err)
			}
			full := filepath.Join(uc.engine.DownloadDir(), paths[0])
			if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
				t.Fatal(err)
			}
			for _, p := range []string{full, full + ".part"} {
				if err := os.WriteFile(p, []byte("test"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			unrelated := filepath.Join(uc.engine.DownloadDir(), "unrelated")
			if err := os.WriteFile(unrelated, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			temporaryFile(t, uc, "last", false)
			if err := uc.SetTemporaryLimit(1); err != nil {
				t.Fatal(err)
			}
			uc.CleanupTemporary("test")
			for _, p := range []string{full, full + ".part"} {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Fatalf("file survived %s: %v", p, err)
				}
			}
			if got, err := repo.GetTorrent(a.Hash); err != nil || got != nil {
				t.Fatalf("record survived: %v %v", got, err)
			}
			if _, err := os.Stat(unrelated); err != nil {
				t.Fatal("unrelated removed", err)
			}
			if got, err := repo.GetTorrent(b.Hash); err != nil || got == nil || got.IsTemporary {
				t.Fatal("permanent removed")
			}
		})
	}
}

func TestTemporaryCleanupRejectsUnsafeAndSharedPaths(t *testing.T) {
	for _, mode := range []string{"symlink", "traversal", "shared", "directory", "reserved", "case-alias"} {
		t.Run(mode, func(t *testing.T) {
			uc, repo, cleanup := setupTorrentUseCase(t)
			defer cleanup()
			if err := uc.SetTemporaryLimit(10); err != nil {
				t.Fatal(err)
			}
			a := temporaryFile(t, uc, "old", false)
			temporaryFile(t, uc, "new", false)
			path := "old"
			switch mode {
			case "symlink":
				outside := filepath.Join(t.TempDir(), "keep")
				if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(uc.engine.DownloadDir(), path)); err != nil {
					t.Skip(err)
				}
			case "directory":
				if err := os.MkdirAll(filepath.Join(uc.engine.DownloadDir(), path), 0700); err != nil {
					t.Fatal(err)
				}
			case "traversal":
				path = "../outside"
			case "reserved":
				path = ".torrent.bolt.db"
			case "shared", "case-alias":
				other := &models.Torrent{Hash: "other", Name: "other", State: models.StatePaused}
				if err := repo.SaveTorrent(other); err != nil {
					t.Fatal(err)
				}
				otherPath := path
				if mode == "case-alias" {
					otherPath = "OLD"
					if err := os.WriteFile(filepath.Join(uc.engine.DownloadDir(), otherPath), []byte("keep"), 0600); err != nil {
						t.Fatal(err)
					}
					if _, err := os.Stat(filepath.Join(uc.engine.DownloadDir(), path)); os.IsNotExist(err) {
						t.Skip("case-sensitive filesystem")
					}
				}
				if err := repo.SaveCleanupPaths(other.Hash, []string{otherPath}); err != nil {
					t.Fatal(err)
				}
			}
			if err := repo.SaveCleanupPaths(a.Hash, []string{path}); err != nil {
				t.Fatal(err)
			}
			if err := uc.SetTemporaryLimit(1); err != nil {
				t.Fatal(err)
			}
			uc.CleanupTemporary("test")
			q, err := repo.TemporaryQueue()
			if err != nil || len(q) != 2 {
				t.Fatalf("unsafe cleanup succeeded: %v %v", q, err)
			}
			if mode == "directory" {
				if err := os.Remove(filepath.Join(uc.engine.DownloadDir(), path)); err != nil {
					t.Fatal(err)
				}
				uc.CleanupTemporary("retry")
				q, _ = repo.TemporaryQueue()
				if len(q) != 1 {
					t.Fatal("retry failed")
				}
			}
		})
	}
}

func TestTemporaryConcurrentSyncAddPinAndTransfer(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	sw := NewSyncWorker(uc.engine, repo)
	ch := make(chan []byte, 100)
	sw.AddClient(ch)
	defer sw.RemoveClient(ch)
	var wg sync.WaitGroup
	for i := 1; i <= 12; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			torrent, err := uc.AddMagnetWithPolicy(fmt.Sprintf("magnet:?xt=urn:btih:%040x", id), TorrentMetadata{}, true)
			if err != nil {
				t.Error(err)
				return
			}
			// Cleanup may win before pin/admission: not found is legitimate.
			_ = uc.MakePermanent(torrent.Hash)
			if release, err := uc.AcquireTransfer(torrent.Hash); err == nil {
				release()
			}
			sw.syncOnce()
			uc.CleanupTemporary("concurrent")
		}(i)
	}
	wg.Wait()
	sw.syncOnce()
	all, err := repo.GetAllTorrents()
	if err != nil {
		t.Fatal(err)
	}
	for _, torrent := range all {
		if torrent.IsTemporary {
			t.Fatal("successful pins were overwritten")
		}
	}
	for len(ch) > 0 {
		var snapshot []map[string]any
		if err := json.Unmarshal(<-ch, &snapshot); err != nil {
			t.Fatal(err)
		}
		for _, torrent := range snapshot {
			if _, ok := torrent["is_temporary"]; !ok {
				t.Fatal("SSE omitted is_temporary")
			}
		}
	}
	// A DB-missing engine entry must never be recreated by the sync worker.
	if _, err := uc.engine.AddInfoHash(testInfoHash); err != nil {
		t.Fatal(err)
	}
	sw.syncOnce()
	if got, _ := repo.GetTorrent(testInfoHash); got != nil {
		t.Fatal("sync resurrected a missing record")
	}
}

func TestTemporaryPartialDeletionSurvivesRestart(t *testing.T) {
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
	uc := NewTorrentUseCase(eng, repository.NewTorrentRepo(db))
	a := temporaryFile(t, uc, "old", false)
	temporaryFile(t, uc, "new", false)
	paths, err := uc.knownStoragePaths(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Delete(a.Hash); err != nil {
		t.Fatal(err)
	}
	if err := uc.repo.SaveCleanupPaths(a.Hash, paths); err != nil {
		t.Fatal(err)
	}
	// The final file was already removed by a prior attempt, .part remains.
	remaining := filepath.Join(dir, paths[0]+".part")
	if err := os.MkdirAll(filepath.Dir(remaining), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remaining, []byte("partial"), 0600); err != nil {
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
	if err := uc.SetTemporaryLimit(1); err != nil {
		t.Fatal(err)
	}
	if err := uc.RestoreTorrents(); err != nil {
		t.Fatal(err)
	}
	uc.CleanupTemporary("startup")
	if got, err := uc.repo.GetTorrent(a.Hash); err != nil || got != nil {
		t.Fatalf("retry record remains: %+v %v", got, err)
	}
	if _, err := os.Stat(remaining); !os.IsNotExist(err) {
		t.Fatalf("remaining file not deleted: %v", err)
	}
}

func TestTemporaryFailedAndRepeatedAddDoNotClean(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	a := temporaryMagnet(t, uc, 1)
	temporaryMagnet(t, uc, 2)
	if err := uc.SetTemporaryLimit(1); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.AddMagnetWithPolicy("invalid", TorrentMetadata{}, true); err == nil {
		t.Fatal("invalid add succeeded")
	}
	temporaryMagnet(t, uc, 2)
	q, err := repo.TemporaryQueue()
	if err != nil || len(q) != 2 || q[0] != a.Hash {
		t.Fatalf("failed/repeated add triggered cleanup: %v %v", q, err)
	}
}

func TestTemporaryCleanupDBFailureKeepsAddSuccessfulAndRetries(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DownloadDir: dir, DBPath: filepath.Join(dir, "hub.db"), TorrentPort: 0}
	db, err := repository.NewSQLiteDB(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	uc := NewTorrentUseCase(eng, repository.NewTorrentRepo(db))
	if err := uc.SetTemporaryLimit(1); err != nil {
		t.Fatal(err)
	}
	a := temporaryFile(t, uc, "old", false)
	paths, err := uc.knownStoragePaths(a)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, paths[0]+".part")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("keep until manifest is durable"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`CREATE TRIGGER fail_manifest BEFORE UPDATE OF cleanup_ready ON torrents WHEN NEW.cleanup_ready = 1 BEGIN SELECT RAISE(ABORT, 'injected DB error'); END`); err != nil {
		t.Fatal(err)
	}
	// Addition must still succeed although cleanup cannot finalize its manifest.
	temporaryFile(t, uc, "new", false)
	got, err := uc.repo.GetTorrent(a.Hash)
	if err != nil || got == nil || !got.IsTemporary {
		t.Fatalf("lost failed candidate: %+v %v", got, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("data removed before durable manifest", err)
	}
	if ready, err := uc.repo.CleanupReady(a.Hash); err != nil || ready {
		t.Fatal("incomplete manifest was marked ready")
	}
	if _, err := db.DB().Exec(`DROP TRIGGER fail_manifest`); err != nil {
		t.Fatal(err)
	}
	uc.CleanupTemporary("retry")
	if got, err := uc.repo.GetTorrent(a.Hash); err != nil || got != nil {
		t.Fatalf("retry failed: %+v %v", got, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("retry left payload: %v", err)
	}
}
