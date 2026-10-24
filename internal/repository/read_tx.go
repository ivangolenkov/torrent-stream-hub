package repository

import (
	"context"
	"database/sql"
)

type sqlReader interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

// BeginTx's lifetime context cancels pool acquisition and rollback. Explicitly
// pass it to each query too, so materialization doesn't start uncancellable work.
type contextReadTx struct {
	*sql.Tx
	ctx context.Context
}

func (tx contextReadTx) Query(query string, args ...any) (*sql.Rows, error) {
	return tx.Tx.QueryContext(tx.ctx, query, args...)
}
func (tx contextReadTx) QueryRow(query string, args ...any) *sql.Row {
	return tx.Tx.QueryRowContext(tx.ctx, query, args...)
}
