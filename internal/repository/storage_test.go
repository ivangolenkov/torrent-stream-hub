package repository

import (
	"path/filepath"
	"testing"
)

func TestStorageMigrationUpgradeAndAliasPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the schema immediately before migration 0005.
	if _, err := db.DB().Exec(`DROP TABLE torrent_storage; DELETE FROM migrations WHERE name='0005_storage_layout.sql'; INSERT INTO torrents(hash,name,size,state) VALUES ('old','legacy',4,'Paused')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	db, err = NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := NewTorrentRepo(db)
	m, err := repo.StorageMapping("old")
	if err != nil || m == nil || m.State != "legacy" || m.Version != 0 || m.Root != "" {
		t.Fatalf("legacy import: %+v %v", m, err)
	}
	m.Version = 1
	m.Root = "payload/owner"
	m.State = "ready"
	if err := repo.SaveStorageMapping(m); err != nil {
		t.Fatal(err)
	}
	if err := repo.ClaimStorageAlias("alias", "old"); err != nil {
		t.Fatal(err)
	}
	if err := repo.ClaimStorageAlias("alias", "old"); err != nil {
		t.Fatal("idempotent alias", err)
	}
	if err := repo.SaveStorageMapping(&StorageMapping{Hash: "other", Version: 1, Root: "payload/other", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ClaimStorageAlias("alias", "other"); err == nil {
		t.Fatal("alias reassigned to another owner")
	}
	alias, err := repo.StorageMapping("alias")
	if err != nil || alias.Root != m.Root {
		t.Fatalf("alias overwritten: %+v %v", alias, err)
	}
}
