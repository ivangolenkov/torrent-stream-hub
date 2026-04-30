package repository

import (
	"database/sql"
	"embed"
	"fmt"
	"sort"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type SQLiteDB struct {
	db *sql.DB
}

func NewSQLiteDB(dsn string) (*SQLiteDB, error) {
	// Enable WAL mode via DSN parameters if not already present
	// or we can execute PRAGMA directly.
	// dsn might be just file path, so let's append params
	// A simple approach is just opening it and running pragmas.

	db, err := sql.Open("sqlite", dsn+"?_journal=WAL&_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Set connection pool limits
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	// Enforce WAL mode
	if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		return nil, fmt.Errorf("failed to enable WAL mode: %w", err)
	}

	// Ensure foreign keys are enforced
	if _, err := db.Exec("PRAGMA foreign_keys=ON;"); err != nil {
		return nil, fmt.Errorf("failed to enable foreign keys: %w", err)
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
