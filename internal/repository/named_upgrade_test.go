package repository

import (
	"path/filepath"
	"strings"
	"testing"

	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
)

func TestNamedStorageUpgradeKeepsPhase41JournalsAndAliases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "phase41.db")
	old := phase40Database(t, path)
	migration, err := migrationFiles.ReadFile("migrations/0014_storage_recovery.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = old.DB().Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	if _, err = old.DB().Exec(`INSERT INTO migrations(name) VALUES('0014_storage_recovery.sql')`); err != nil {
		t.Fatal(err)
	}
	oldRoot := payload.Root(strings.Repeat("a", 40))
	manifest := `{"Entries":[{"Source":"old.part","Target":"old"}],"Complete":true}`
	journal := `{"metadata_path":"old","choice":"final","backup":".tsh-recovery/preserved-copy","alternatives":[{"path":".tsh-recovery/earlier-copy"}]}`
	for _, state := range []string{"ready", "alias", "legacy", "blocked", "moving", "cancel"} {
		if err := NewTorrentRepo(old).SaveTorrent(&models.Torrent{Hash: state, State: models.StatePaused}); err != nil {
			t.Fatal(err)
		}
		root, version := oldRoot, 1
		if state != "ready" && state != "alias" {
			root = ""
			version = 0
		}
		mappingState := state
		if state == "alias" || state == "cancel" {
			mappingState = "ready"
		}
		if _, err := old.DB().Exec(`INSERT INTO torrent_storage(hash,version,root,state,manifest) VALUES(?,?,?,?,?)`, state, version, root, mappingState, manifest); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := old.DB().Exec(`UPDATE torrents SET deletion_mode='payload',cleanup_ready=1,cleanup_paths='["remaining"]' WHERE hash='cancel'`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.DB().Exec(`UPDATE storage_recovery SET version=7,stage='cancel_requested',journal=? WHERE hash='cancel'`, journal); err != nil {
		t.Fatal(err)
	}
	if _, err := old.DB().Exec(`INSERT INTO storage_recovery(hash,record_id,version,reason,stage,journal) SELECT hash,record_id,9,'legacy_ambiguous','preserved',? FROM torrent_intents WHERE hash='blocked'`, journal); err != nil {
		t.Fatal(err)
	}
	old.Close()
	var owner string
	for attempt := 0; attempt < 2; attempt++ {
		db, err := NewSQLiteDB(path)
		if err != nil {
			t.Fatal(err)
		}
		repo := NewTorrentRepo(db)
		ready, _ := repo.StorageMapping("ready")
		alias, _ := repo.StorageMapping("alias")
		if ready.Root != oldRoot || ready.Version != 1 || ready.Prefix != "" || ready.StorageID == "" || ready.StorageID != alias.StorageID || ready.Revision != 1 {
			t.Fatalf("upgrade changed layout/aliases: %+v %+v", ready, alias)
		}
		if owner != "" && owner != ready.StorageID {
			t.Fatal("reopen reassigned owner")
		}
		owner = ready.StorageID
		for _, hash := range []string{"ready", "alias", "legacy", "blocked", "moving", "cancel"} {
			mapping, err := repo.StorageMapping(hash)
			if err != nil || mapping.Manifest != manifest {
				t.Fatal("upgrade rewrote old manifest", hash, err)
			}
		}
		for _, hash := range []string{"blocked", "cancel"} {
			rec, err := repo.Recovery(hash)
			if err != nil || rec.Journal != journal || hash == "cancel" && (rec.Stage != "cancel_requested" || rec.Version != 7) || hash == "blocked" && (rec.Stage != "preserved" || rec.Version != 9) {
				t.Fatalf("upgrade changed journal: %+v %v", rec, err)
			}
		}
		var mode, paths string
		var cleanup bool
		if err := db.DB().QueryRow(`SELECT deletion_mode,cleanup_ready,cleanup_paths FROM torrents WHERE hash='cancel'`).Scan(&mode, &cleanup, &paths); err != nil || mode != "payload" || !cleanup || paths != `["remaining"]` {
			t.Fatal("lost pending deletion", mode, cleanup, paths, err)
		}
		db.Close()
	}
}
