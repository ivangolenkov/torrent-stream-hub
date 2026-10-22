package usecase

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
)

func TestDeletionFinalDatabaseFailureSurvivesReopen(t *testing.T) {
	for _, payload := range []bool{false, true} {
		name := "keep"
		if payload {
			name = "payload"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := &config.Config{DownloadDir: dir, DBPath: filepath.Join(dir, "hub.db"), BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true}
			open := func() (*TorrentUseCase, *repository.SQLiteDB) {
				t.Helper()
				db, err := repository.NewSQLiteDB(cfg.DBPath)
				if err != nil {
					t.Fatal(err)
				}
				e, err := engine.New(cfg)
				if err != nil {
					db.Close()
					t.Fatal(err)
				}
				return NewTorrentUseCase(e, repository.NewTorrentRepo(db)), db
			}
			uc, db := open()
			tor, full := completedSafetyFile(t, uc, "delete-"+name, true)
			if _, err := db.DB().Exec(`CREATE TRIGGER fail_final_delete BEFORE DELETE ON torrents BEGIN SELECT RAISE(FAIL,'injected final delete failure'); END`); err != nil {
				t.Fatal(err)
			}
			if err := uc.Delete(tor.Hash, payload); err == nil {
				t.Fatal("delete reported success after failed DB transaction")
			}
			saved, err := uc.repo.GetTorrent(tor.Hash)
			if err != nil {
				t.Fatal(err)
			}
			if saved.DeletionMode != name || saved.DeletionError == "" || saved.State != models.StatePaused {
				t.Fatalf("intent missing: %+v", saved)
			}
			if payload {
				if _, err := os.Stat(full); !os.IsNotExist(err) {
					t.Fatal("payload not removed before final transaction", err)
				}
			} else if _, err := os.Stat(full); err != nil {
				t.Fatal("keep-data deletion unlinked payload", err)
			}
			// Even a stale caller bypassing SyncWorker cannot erase durable intent.
			saved.State = models.StateDownloading
			if err := uc.repo.SaveTorrent(saved); err != nil {
				t.Fatal(err)
			}
			uc.engine.Close()
			db.Close()
			uc, db = open()
			defer db.Close()
			defer uc.engine.Close()
			if err := uc.RestoreTorrents(); err != nil {
				t.Fatal(err)
			}
			if uc.engine.GetRawTorrent(tor.Hash) != nil {
				t.Fatal("restart restored pending deletion")
			}
			if err := uc.Resume(tor.Hash); !errors.Is(err, ErrStorageConflict) {
				t.Fatal("Resume bypassed intent", err)
			}
			if _, err := uc.AddMagnet("magnet:?xt=urn:btih:" + tor.Hash); !errors.Is(err, ErrStorageConflict) {
				t.Fatal("re-add bypassed intent", err)
			}
			if _, err := uc.AcquireTransfer(tor.Hash); !errors.Is(err, ErrStorageConflict) {
				t.Fatal("transfer bypassed intent", err)
			}
			if err := uc.Delete(tor.Hash, !payload); !errors.Is(err, ErrStorageConflict) {
				t.Fatal("retry changed durable deletion mode", err)
			}
			if _, err := db.DB().Exec(`DROP TRIGGER fail_final_delete`); err != nil {
				t.Fatal(err)
			}
			if err := uc.Delete(tor.Hash, payload); err != nil {
				t.Fatal(err)
			}
			if saved, err := uc.repo.GetTorrent(tor.Hash); err != nil || saved != nil {
				t.Fatal("retry did not finalize deletion", err)
			}
			mapping, err := uc.repo.StorageMapping(tor.Hash)
			if err != nil {
				t.Fatal(err)
			}
			if payload && mapping != nil {
				t.Fatal("deleted payload retained orphan mapping")
			}
			if !payload {
				if mapping == nil {
					t.Fatal("keep-data deletion lost namespace reservation")
				}
				if _, err := os.Stat(full); err != nil {
					t.Fatal("retry removed kept data", err)
				}
			}
		})
	}
}

func TestDeletionIntentFailureDoesNotDropRuntime(t *testing.T) {
	for _, payload := range []bool{false, true} {
		t.Run(map[bool]string{false: "keep", true: "payload"}[payload], func(t *testing.T) {
			uc, _, cleanup := setupTorrentUseCase(t)
			defer cleanup()
			tor := temporaryFile(t, uc, "intent-failure", false)
			db, err := repository.NewSQLiteDB(filepath.Join(uc.engine.DownloadDir(), "hub.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.DB().Exec(`CREATE TRIGGER fail_intent BEFORE UPDATE OF cleanup_paths ON torrents BEGIN SELECT RAISE(FAIL,'injected intent failure'); END`); err != nil {
				t.Fatal(err)
			}
			before := uc.engine.GetRawTorrent(tor.Hash)
			if err := uc.Delete(tor.Hash, payload); err == nil {
				t.Fatal("intent failure ignored")
			}
			if uc.engine.GetRawTorrent(tor.Hash) != before {
				t.Fatal("Drop preceded durable intent")
			}
			saved, err := uc.repo.GetTorrent(tor.Hash)
			if err != nil || saved.DeletionMode != "" {
				t.Fatal("failed intent published", err)
			}
		})
	}
}

func TestDeletionPartialUnlinkAndStoppedEngineFailure(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor, full := completedSafetyFile(t, uc, "partial-unlink", true)
	if err := os.WriteFile(full+".part", []byte("remaining part"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	uc.payloadRemove = func(root *os.Root, path string) error {
		calls++
		if calls == 2 {
			return syscall.EACCES
		}
		return root.Remove(path)
	}
	if err := uc.Delete(tor.Hash, true); !errors.Is(err, syscall.EACCES) {
		t.Fatal("unlink failure ignored", err)
	}
	if _, err := os.Stat(full); !os.IsNotExist(err) {
		t.Fatal("test did not interrupt after first unlink", err)
	}
	if _, err := os.Stat(full + ".part"); err != nil {
		t.Fatal("part disappeared", err)
	}
	saved, err := repo.GetTorrent(tor.Hash)
	if err != nil || saved.DeletionMode != "payload" || saved.DeletionError == "" {
		t.Fatal("partial failure lost intent", err)
	}
	uc.payloadRemove = nil
	if err := uc.Delete(tor.Hash, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(full + ".part"); !os.IsNotExist(err) {
		t.Fatal("retry did not finish", err)
	}
	// Engine refusal after intent commit must also preserve pending state.
	tor, full = completedSafetyFile(t, uc, "stopped-delete", true)
	uc.engine.Close()
	if err := uc.Delete(tor.Hash, true); err == nil {
		t.Fatal("stopped engine error ignored")
	}
	saved, err = repo.GetTorrent(tor.Hash)
	if err != nil || saved.DeletionMode != "payload" {
		t.Fatal("failed Drop lost intent", err)
	}
	if _, err := os.Stat(full); err != nil {
		t.Fatal("unlinked after engine stop failure", err)
	}
}
