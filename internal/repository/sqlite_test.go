package repository

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLitePragmasOnEveryConnection(t *testing.T) {
	db, err := NewSQLiteDB(filepath.Join(t.TempDir(), "pool space.db") + "?cache=private")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	var connections []*sql.Conn
	defer func() {
		for _, c := range connections {
			c.Close()
		}
	}()
	for i := 0; i < 8; i++ {
		c, err := db.DB().Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, c)
		assertSQLitePragmas(t, c)
	}
	// Force new physical connections, not just reuse of the initial connection.
	db.DB().SetMaxIdleConns(0)
	for _, c := range connections {
		c.Close()
	}
	connections = nil
	c, err := db.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	assertSQLitePragmas(t, c)
	if _, err := c.ExecContext(ctx, `INSERT INTO files (hash,"index",path,size) VALUES ('missing',0,'x',1)`); err == nil {
		t.Fatal("foreign key violation accepted")
	}
}

func assertSQLitePragmas(t *testing.T, c *sql.Conn) {
	t.Helper()
	for name, want := range map[string]int{"foreign_keys": 1, "busy_timeout": 5000} {
		var got int
		if err := c.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&got); err != nil || got != want {
			t.Fatalf("%s=%d want=%d err=%v", name, got, want, err)
		}
	}
	var mode string
	if err := c.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("mode=%s err=%v", mode, err)
	}
}

func TestSQLiteBusyTimeoutAllowsLockRelease(t *testing.T) {
	db, err := NewSQLiteDB(filepath.Join(t.TempDir(), "locks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO torrents(hash,name,size,state) VALUES ('one','one',0,'Paused')`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := db.DB().Exec(`INSERT INTO torrents(hash,name,size,state) VALUES ('two','two',0,'Paused')`)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("did not wait for lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("writer did not finish within busy timeout")
	}
}

func TestSQLiteMemoryAndInvalidDSN(t *testing.T) {
	for _, dsn := range []string{":memory:", "file::memory:?cache=shared", "file:phase34?mode=memory&cache=shared"} {
		db, err := NewSQLiteDB(dsn)
		if err != nil {
			t.Fatal(err)
		}
		var mode string
		err = db.DB().QueryRow("PRAGMA journal_mode").Scan(&mode)
		db.Close()
		if err != nil || mode != "memory" {
			t.Fatalf("%s: mode=%s err=%v", dsn, mode, err)
		}
	}
	if db, err := NewSQLiteDB("file:bad?x=%xx"); err == nil || db != nil {
		t.Fatal("invalid query accepted")
	}
}
