package repository

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"torrent-stream-hub/internal/payload"
)

func TestStorageLayoutOwnerRevisionAndRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "layout.db")
	db, err := NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	repo := NewTorrentRepo(db)
	if err := repo.EnsureStorageMapping(&StorageMapping{Hash: "v1", Version: 2, State: "reserved"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ClaimStorageAlias("v2", "v1"); err != nil {
		t.Fatal(err)
	}
	before, _ := repo.StorageMapping("v1")
	alias, _ := repo.StorageMapping("v2")
	if before.StorageID == "" || before.StorageID != alias.StorageID {
		t.Fatal("independent alias owners")
	}
	first := payload.Layout{Version: 2, Root: filepath.FromSlash("payload/Series"), Prefix: "Original"}
	if err := repo.SetStorageLayout(context.Background(), before, "", first); err != nil {
		t.Fatal(err)
	}
	current, _ := repo.StorageMapping("v1")
	alias, _ = repo.StorageMapping("v2")
	if current.Revision != before.Revision+1 || alias.Revision != current.Revision || current.Root != alias.Root || current.Prefix != alias.Prefix {
		t.Fatalf("non-atomic aliases %+v %+v", current, alias)
	}
	if err := repo.SetStorageLayout(context.Background(), before, "", payload.Layout{Version: 2, Root: filepath.FromSlash("payload/Stale")}); !errors.Is(err, ErrStateConflict) {
		t.Fatal("stale mapping accepted", err)
	}
	if err := repo.EnsureStorageMapping(&StorageMapping{Hash: "other", Version: 2, Root: filepath.FromSlash("payload/Occupied"), State: "ready"}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{filepath.FromSlash("payload/OCCUPIED"), filepath.FromSlash("payload/occupied/child"), "payload"} {
		if err := repo.SetStorageLayout(context.Background(), current, "", payload.Layout{Version: 2, Root: target}); !errors.Is(err, ErrStateConflict) {
			t.Fatal("overlap accepted", target, err)
		}
	}
	if _, err := db.DB().Exec(`CREATE TRIGGER fail_alias BEFORE UPDATE OF root ON torrent_storage WHEN OLD.hash='v2' BEGIN SELECT RAISE(ABORT,'injected');END;`); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetStorageLayout(context.Background(), current, "", payload.Layout{Version: 2, Root: filepath.FromSlash("payload/Rollback")}); err == nil {
		t.Fatal("failed update committed")
	}
	db.Close()
	db, err = NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo = NewTorrentRepo(db)
	reopened, _ := repo.StorageMapping("v1")
	alias, _ = repo.StorageMapping("v2")
	if *reopened != *current || alias.Root != current.Root || alias.Revision != current.Revision {
		t.Fatalf("partial group survived reopen: %+v %+v", reopened, alias)
	}
}
