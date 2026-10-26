package repository

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"modernc.org/sqlite"
)

const sqliteDriverName = "tsh-sqlite"

func init() {
	// modernc sorts DSN pragmas, so appending FULL does not override a caller's
	// synchronous(OFF). This private driver's hook runs AFTER DSN configuration
	// on every new/replacement connection, without affecting other SQLite users.
	driver := &sqlite.Driver{}
	driver.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, _ string) error {
		_, err := conn.ExecContext(context.Background(), "PRAGMA synchronous=FULL", nil)
		return err
	})
	sql.Register(sqliteDriverName, driver)
}

//go:embed migrations/*.sql
var migrationFiles embed.FS

type SQLiteDB struct {
	db *sql.DB
}

func NewSQLiteDB(dsn string) (_ *SQLiteDB, retErr error) {
	// modernc applies _pragma on EVERY connection, including pool replacements.
	base, query, _ := strings.Cut(dsn, "?")
	params, err := url.ParseQuery(query)
	if err != nil {
		return nil, fmt.Errorf("invalid SQLite DSN: %w", err)
	}
	memory := base == ":memory:" || base == "file::memory:" || params.Get("mode") == "memory"
	params.Add("_pragma", "busy_timeout(5000)")
	params.Add("_pragma", "foreign_keys(1)")
	if !memory {
		params.Add("_pragma", "journal_mode(WAL)")
	}
	// Safety-critical resume revocations must survive before a payload write.
	// The connection hook enforces this AFTER the driver's sorted DSN pragmas.
	params.Add("_pragma", "synchronous(FULL)")
	db, err := sql.Open(sqliteDriverName, base+"?"+params.Encode())
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = db.Close()
		}
	}()

	// Set connection pool limits
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	if memory {
		// Private in-memory databases are connection-local; also keep named memory
		// databases alive until Close, without relying on shared-cache semantics.
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	if !memory {
		var mode string
		if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || !strings.EqualFold(mode, "wal") {
			return nil, fmt.Errorf("SQLite WAL unavailable (mode=%q): %v", mode, err)
		}
	}
	var synchronous int
	if err := db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil || synchronous != 2 {
		return nil, fmt.Errorf("SQLite FULL synchronous unavailable (mode=%d): %v", synchronous, err)
	}

	sqliteDB := &SQLiteDB{db: db}

	if err := sqliteDB.runMigrations(); err != nil {
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	return sqliteDB, nil
}

func (s *SQLiteDB) DB() *sql.DB {
	return s.db
}

func (s *SQLiteDB) Close() error {
	return s.db.Close()
}

func (s *SQLiteDB) runMigrations() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS migrations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	if err != nil {
		return err
	}

	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return err
	}

	var files []string
	for _, entry := range entries {
		if !entry.IsDir() {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)

	for _, file := range files {
		var exists int
		err := s.db.QueryRow("SELECT 1 FROM migrations WHERE name = ?", file).Scan(&exists)
		if err == sql.ErrNoRows {
			// Run migration
			content, err := migrationFiles.ReadFile("migrations/" + file)
			if err != nil {
				return fmt.Errorf("failed to read migration %s: %w", file, err)
			}

			tx, err := s.db.Begin()
			if err != nil {
				return err
			}

			if _, err := tx.Exec(string(content)); err != nil {
				tx.Rollback()
				return fmt.Errorf("failed to execute migration %s: %w", file, err)
			}

			if _, err := tx.Exec("INSERT INTO migrations (name) VALUES (?)", file); err != nil {
				tx.Rollback()
				return err
			}

			if err := tx.Commit(); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}

	return nil
}
