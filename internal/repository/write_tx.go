package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"time"
)

// Reserve SQLite's single writer BEFORE reading command preconditions. Deferred
// read->write upgrades can return SQLITE_BUSY_SNAPSHOT without honoring the busy
// timeout after an independent writer commits. This is a short DB transaction,
// never an application/I/O lock; read-only snapshot transactions stay deferred.
func (db *SQLiteDB) BeginWrite(contexts ...context.Context) (*writeTx, error) {
	ctx := context.Background()
	cancellable := len(contexts) > 0 && contexts[0] != nil
	if cancellable {
		ctx = contexts[0]
	}
	conn, err := db.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	reset := func() {
		if cancellable {
			_, _ = conn.ExecContext(context.Background(), "PRAGMA busy_timeout=5000")
		}
	}
	if cancellable {
		if _, err = conn.ExecContext(ctx, "PRAGMA busy_timeout=50"); err != nil {
			reset()
			conn.Close()
			return nil, err
		}
	}
	for {
		_, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE")
		if err == nil {
			break
		}
		var code interface{ Code() int }
		if !cancellable || !errors.As(err, &code) || code.Code()&255 != 5 {
			reset()
			conn.Close()
			return nil, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			reset()
			conn.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return &writeTx{conn: conn, ctx: ctx, resetBusy: cancellable}, nil
}

type sqlWriter interface {
	Prepare(string) (*sql.Stmt, error)
	Exec(string, ...any) (sql.Result, error)
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

type writeTx struct {
	conn      *sql.Conn
	done      bool
	ctx       context.Context
	resetBusy bool
}

func (tx *writeTx) Prepare(query string) (*sql.Stmt, error) {
	return tx.conn.PrepareContext(tx.ctx, query)
}

func (tx *writeTx) Exec(query string, args ...any) (sql.Result, error) {
	return tx.conn.ExecContext(tx.ctx, query, args...)
}
func (tx *writeTx) Query(query string, args ...any) (*sql.Rows, error) {
	return tx.conn.QueryContext(tx.ctx, query, args...)
}
func (tx *writeTx) QueryRow(query string, args ...any) *sql.Row {
	return tx.conn.QueryRowContext(tx.ctx, query, args...)
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
	if tx.resetBusy {
		_, _ = tx.conn.ExecContext(context.Background(), "PRAGMA busy_timeout=5000")
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
	if tx.resetBusy {
		_, _ = tx.conn.ExecContext(context.Background(), "PRAGMA busy_timeout=5000")
	}
	closeErr := tx.conn.Close()
	if err != nil {
		return err
	}
	return closeErr
}
