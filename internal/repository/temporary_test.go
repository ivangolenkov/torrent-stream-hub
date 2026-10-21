package repository

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	"torrent-stream-hub/internal/models"
)

func TestTemporaryPolicyAndFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	r := NewTorrentRepo(db)
	t.Cleanup(func() { db.Close() })
	for _, hash := range []string{"z", "a", "m"} {
		if err := r.SaveTorrentWithPolicy(&models.Torrent{Hash: hash, Name: hash, State: models.StateQueued}, true); err != nil {
			t.Fatal(err)
		}
	}
	// All timestamp ties must still follow insertion, not lexical order.
	if _, err := db.DB().Exec(`UPDATE torrents SET created_at = '2026-01-01 00:00:00'`); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveTorrent(&models.Torrent{Hash: "z", Name: "z", State: models.StateDownloading}); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveTorrentWithPolicy(&models.Torrent{Hash: "z", Name: "z", State: models.StateQueued}, true); err != nil {
		t.Fatal(err)
	}
	q, err := r.TemporaryQueue()
	if err != nil || !reflect.DeepEqual(q, []string{"z", "a", "m"}) {
		t.Fatalf("queue=%v err=%v", q, err)
	}
	if err := r.SaveTorrentWithPolicy(&models.Torrent{Hash: "a", Name: "a", State: models.StateQueued}, false); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveTorrentWithPolicy(&models.Torrent{Hash: "a", Name: "a", State: models.StateQueued}, true); err != nil {
		t.Fatal(err)
	}
	a, err := r.GetTorrent("a")
	if err != nil || a.IsTemporary {
		t.Fatalf("permanent downgraded: %+v %v", a, err)
	}
	if err := r.DeleteTorrent("z"); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveTorrentWithPolicy(&models.Torrent{Hash: "z", Name: "z", State: models.StateQueued}, true); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	r = NewTorrentRepo(db)
	q, err = r.TemporaryQueue()
	if err != nil || !reflect.DeepEqual(q, []string{"m", "z"}) {
		t.Fatalf("after restart queue=%v err=%v", q, err)
	}
	for i := 0; i < 2; i++ {
		found, err := r.MakePermanent("m")
		if err != nil || !found {
			t.Fatalf("pin: %v %v", found, err)
		}
	}
	if found, err := r.MakePermanent("missing"); found || err != nil {
		t.Fatalf("missing: %v %v", found, err)
	}
}

func TestTemporaryMigrationPreservesExistingData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE migrations (name TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"0001_init.sql", "0002_add_torrent_source_uri.sql", "0003_add_torrent_metadata.sql"} {
		data, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(string(data)); err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(`INSERT INTO migrations(name) VALUES (?)`, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := old.Exec(`INSERT INTO torrents(hash,name,size,state,error,title) VALUES ('old','Old',1024,'Paused','','Keep title'); INSERT INTO files(hash,"index",path,size) VALUES ('old',0,'video',1024)`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	db, err := NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewTorrentRepo(db)
	torrent, err := r.GetTorrent("old")
	if err != nil || torrent == nil || torrent.IsTemporary || torrent.Title != "Keep title" || len(torrent.Files) != 1 {
		t.Fatalf("migration lost data: %+v %v", torrent, err)
	}
	var journal string
	if err := db.DB().QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("WAL=%s err=%v", journal, err)
	}
}
