package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
)

// Reserve SQLite's single writer BEFORE reading command preconditions. Deferred
// read->write upgrades can return SQLITE_BUSY_SNAPSHOT without honoring the busy
// timeout after an independent writer commits. This is a short DB transaction,
// never an application/I/O lock; read-only snapshot transactions stay deferred.
func (db *SQLiteDB) BeginWrite() (*writeTx, error) {
	conn, err := db.db.Conn(context.Background())
	if err != nil {
		return nil, err
	}
	if _, err = conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		conn.Close()
		return nil, err
	}
	return &writeTx{conn: conn}, nil
}

type sqlWriter interface {
	Prepare(string) (*sql.Stmt, error)
	Exec(string, ...any) (sql.Result, error)
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

type writeTx struct {
	conn *sql.Conn
	done bool
}

func (tx *writeTx) Prepare(query string) (*sql.Stmt, error) {
	return tx.conn.PrepareContext(context.Background(), query)
}

func (tx *writeTx) Exec(query string, args ...any) (sql.Result, error) {
	return tx.conn.ExecContext(context.Background(), query, args...)
}
func (tx *writeTx) Query(query string, args ...any) (*sql.Rows, error) {
	return tx.conn.QueryContext(context.Background(), query, args...)
}
func (tx *writeTx) QueryRow(query string, args ...any) *sql.Row {
	return tx.conn.QueryRowContext(context.Background(), query, args...)
}
func (tx *writeTx) Commit() error {
	if tx.done {
		return sql.ErrTxDone
	}
	tx.done = true
	_, err := tx.conn.ExecContext(context.Background(), "COMMIT")
	if err != nil {
		if _, rollbackErr := tx.conn.ExecContext(context.Background(), "ROLLBACK"); rollbackErr != nil {
			_ = tx.conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}
	closeErr := tx.conn.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func (tx *writeTx) Rollback() error {
	if tx.done {
		return sql.ErrTxDone
	}
	tx.done = true
	_, err := tx.conn.ExecContext(context.Background(), "ROLLBACK")
	if err != nil {
		_ = tx.conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	closeErr := tx.conn.Close()
	if err != nil {
		return err
	}
	return closeErr
}
