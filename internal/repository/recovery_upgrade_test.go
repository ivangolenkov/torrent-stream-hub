package repository

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"torrent-stream-hub/internal/models"
)

func TestRecoveryStaleMigrationCannotChangeReaddedMapping(t *testing.T) {
	db, err := NewSQLiteDB(filepath.Join(t.TempDir(), "fenced.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := NewTorrentRepo(db)
	if err := repo.SaveTorrent(&models.Torrent{Hash: "same", State: models.StatePaused}); err != nil {
		t.Fatal(err)
	}
	old, _ := repo.GetIntent("same")
	if err := repo.DeleteTorrent("same"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveTorrent(&models.Torrent{Hash: "same", State: models.StatePaused}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveStorageMapping(&StorageMapping{Hash: "same", State: "ready", Root: "new"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveStorageMappingForRecord(&StorageMapping{Hash: "same", State: "moving", Root: "old"}, old.Version.RecordID); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("stale write accepted: %v", err)
	}
	m, _ := repo.StorageMapping("same")
	if m.Root != "new" || m.State != "ready" {
		t.Fatal("new mapping overwritten")
	}
}

func phase40Database(t *testing.T, path string) *SQLiteDB {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	if _, err := raw.Exec(`PRAGMA foreign_keys=ON;PRAGMA journal_mode=WAL;CREATE TABLE migrations(id INTEGER PRIMARY KEY AUTOINCREMENT,name TEXT NOT NULL UNIQUE,applied_at DATETIME DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Compare(entry.Name(), "0014_storage_recovery.sql") >= 0 {
			continue
		}
		content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec(string(content)); err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec(`INSERT INTO migrations(name) VALUES(?)`, entry.Name()); err != nil {
			t.Fatal(err)
		}
	}
	return &SQLiteDB{db: raw}
}

func TestRecoveryUpgradePreservesIntentsAndUnknownManifests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old := phase40Database(t, path)
	repo := NewTorrentRepo(old)
	for _, state := range []string{"ready", "legacy", "blocked", "moving"} {
		if err := repo.SaveTorrent(&models.Torrent{Hash: state, State: models.StatePaused}); err != nil {
			t.Fatal(err)
		}
		if err := repo.SaveStorageMapping(&StorageMapping{Hash: state, State: state, Manifest: `{"Root":"payload/legacy","Entries":[]}`}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.BeginCleanup("ready", []string{"remaining"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveCleanupPaths("ready", []string{"remaining"}); err != nil {
		t.Fatal(err)
	}
	var beforeID string
	if err := old.DB().QueryRow(`SELECT record_id FROM torrent_intents WHERE hash='ready'`).Scan(&beforeID); err != nil {
		t.Fatal(err)
	}
	old.Close()
	db, err := NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	repo = NewTorrentRepo(db)
	rec, err := repo.Recovery("ready")
	if err != nil || rec == nil || rec.RecordID != beforeID || rec.RemovedData != "unknown" || rec.Mode != "payload" {
		t.Fatalf("unsafe imported recovery: %+v %v", rec, err)
	}
	for _, state := range []string{"ready", "legacy", "blocked", "moving"} {
		m, err := repo.StorageMapping(state)
		if err != nil || m.State != state || m.Manifest == "" {
			t.Fatalf("mapping/journal changed: %+v %v", m, err)
		}
	}
	db.Close()
	db, err = NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rec, err = NewTorrentRepo(db).Recovery("ready")
	if err != nil || rec.RecordID != beforeID || rec.RemovedData != "unknown" {
		t.Fatal("repeated upgrade reset intent", err)
	}
}

func TestRecoveryUpgradeRollsBackAndCanRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.db")
	old := phase40Database(t, path)
	if err := NewTorrentRepo(old).SaveTorrent(&models.Torrent{Hash: "keep", State: models.StatePaused}); err != nil {
		t.Fatal(err)
	}
	// Fail after the first CREATE TABLE/triggers in migration 14, not before its
	// transaction starts. All newly created ownership objects must roll back.
	if _, err := old.DB().Exec(`CREATE TABLE storage_recovery(injected INTEGER)`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	if db, err := NewSQLiteDB(path); err == nil {
		db.Close()
		t.Fatal("injected upgrade unexpectedly succeeded")
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='storage_catalog_version'`).Scan(&n); err != nil || n != 0 {
		t.Fatal("partial migration escaped rollback", n, err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM migrations WHERE name='0014_storage_recovery.sql'`).Scan(&n); err != nil || n != 0 {
		t.Fatal("failed migration marked applied", n, err)
	}
	if _, err := raw.Exec(`DROP TABLE storage_recovery`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	db, err := NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tor, err := NewTorrentRepo(db).GetTorrent("keep")
	if err != nil || tor == nil {
		t.Fatal("upgrade lost record", err)
	}
}
