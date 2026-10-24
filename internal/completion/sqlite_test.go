package completion_test

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"torrent-stream-hub/internal/completion"
	"torrent-stream-hub/internal/repository"
)

func TestSQLiteHintsReopenAndClose(t *testing.T) {
	db, err := repository.NewSQLiteDB(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c, err := completion.New(db.DB())
	if err != nil {
		t.Fatal(err)
	}
	key := metainfo.PieceKey{Index: 3}
	key.InfoHash[0] = 1
	if err = c.Set(key, true); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Get(key); err != nil || !got.Ok || !got.Complete {
		t.Fatal(got, err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if err = c.Set(key, false); !errors.Is(err, sql.ErrConnDone) {
		t.Fatal(err)
	}
	c, err = completion.New(db.DB())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.Get(key); err != nil || !got.Complete {
		t.Fatal(got, err)
	}
	if err = c.Set(key, false); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	var value bool
	if err = db.DB().QueryRow(`SELECT complete FROM piece_completion WHERE infohash=? AND piece=?`, key.InfoHash[:], key.Index).Scan(&value); err != nil || value {
		t.Fatal(value, err)
	}
}

func TestSQLiteHintsBoundedWithBusyDatabase(t *testing.T) {
	db, err := repository.NewSQLiteDB(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c, err := completion.New(db.DB())
	if err != nil {
		t.Fatal(err)
	}
	db.DB().SetMaxOpenConns(1)
	tx, err := db.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20000; i++ {
		if err := c.Set(metainfo.PieceKey{Index: i}, true); err != nil {
			t.Fatal(err)
		}
	}
	s := c.Status()
	if s.Cached > completion.CacheEntries || s.Pending > completion.PendingEntries || s.Dropped == 0 {
		t.Fatal(s)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteHintsPersistenceErrorIsVisible(t *testing.T) {
	db, err := repository.NewSQLiteDB(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c, err := completion.New(db.DB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`CREATE TRIGGER fail_hint BEFORE INSERT ON piece_completion BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if err := c.Set(metainfo.PieceKey{}, true); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err == nil {
		t.Fatal("lost persistence error")
	}
	if s := c.Status(); s.Dropped != 1 || s.Error == "" {
		t.Fatal(s)
	}
}

func TestSQLiteHintsReadOnlyAndFullFailures(t *testing.T) {
	for _, mode := range []string{"read-only", "full"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hub.db")
			db, err := repository.NewSQLiteDB(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			sqlDB := db.DB()
			sqlDB.SetMaxOpenConns(1)
			if mode == "read-only" {
				// query_only returns SQLITE_READONLY without relying on process privileges.
				if _, err := sqlDB.Exec("PRAGMA query_only=ON"); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := sqlDB.Exec("CREATE TABLE completion_disk_filler (payload BLOB)"); err != nil {
					t.Fatal(err)
				}
				var pages int
				if err := sqlDB.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
					t.Fatal(err)
				}
				if _, err := sqlDB.Exec(fmt.Sprintf("PRAGMA max_page_count=%d", pages)); err != nil {
					t.Fatal(err)
				}
				// Force a real SQLITE_FULL in the hint transaction, including its trigger.
				if _, err := sqlDB.Exec("CREATE TRIGGER fill_hint BEFORE INSERT ON piece_completion BEGIN INSERT INTO completion_disk_filler VALUES(zeroblob(1048576)); END"); err != nil {
					t.Fatal(err)
				}
			}
			c, err := completion.New(sqlDB)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Set(metainfo.PieceKey{Index: 1}, true); err != nil {
				t.Fatal(err)
			}
			if err := c.Close(); err == nil {
				t.Fatal("missing storage failure")
			}
			if status := c.Status(); status.Dropped != 1 || status.Error == "" {
				t.Fatal(status)
			}
		})
	}
}
