package repository

import (
	"testing"
	"time"
)

func TestImmediateWriterDoesNotBlockWALSnapshotReads(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	tx, err := db.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO torrents(hash,name,size,state) VALUES('held-writer','held',0,'Paused')`); err != nil {
		t.Fatal(err)
	}
	read := make(chan error, 1)
	go func() { _, err := NewTorrentRepo(db).GetAllTorrents(); read <- err }()
	select {
	case err := <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		tx.Rollback()
		t.Fatal("a read-only snapshot waited for an uncommitted writer")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	next, err := db.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestStorageAliasCannotReplaceConcurrentRootClaim(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	r := NewTorrentRepo(db)
	ready := make(chan struct{})
	results := make(chan error, 2)
	for _, root := range []string{"payload/owner-A", "payload/owner-B"} {
		go func(root string) {
			<-ready
			results <- r.SaveStorageAlias(&StorageMapping{Hash: "hybrid-alias", Root: root, Version: 2, State: "ready"})
		}(root)
	}
	close(ready)
	errors := 0
	for range 2 {
		if <-results != nil {
			errors++
		}
	}
	if errors != 1 {
		t.Fatal("alias claim overwrote another root", errors)
	}
	before, err := r.StorageMapping("hybrid-alias")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureStorageMapping(&StorageMapping{Hash: "hybrid-alias", State: "legacy"}); err != nil {
		t.Fatal(err)
	}
	after, err := r.StorageMapping("hybrid-alias")
	if err != nil || *before != *after {
		t.Fatal("legacy initialization replaced a claim", before, after, err)
	}
}
