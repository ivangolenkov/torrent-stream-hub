package repository

import (
	"context"
	"database/sql"

	"torrent-stream-hub/internal/fastresume"
)

// ResumeOwnersAfter visits at most one bounded page of canonical storage owners.
// It neither loads the library read model nor decodes checkpoint payloads. The
// cursor is storage identity, so aliases cannot consume duplicate queue slots.
func (r *TorrentRepo) ResumeOwnersAfter(ctx context.Context, cursor string) ([]string, error) {
	rows, err := r.db.DB().QueryContext(ctx, `SELECT DISTINCT s.storage_id FROM torrent_storage s JOIN torrent_intents i ON i.hash=s.hash WHERE s.storage_id>? ORDER BY s.storage_id LIMIT ?`, cursor, fastresume.MaxPendingOwners)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var owners []string
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return nil, err
		}
		owners = append(owners, owner)
	}
	return owners, rows.Err()
}

// CheckResumeAdmission revalidates ownership/journals in a coherent read-only
// snapshot, without creating a head, decoding a bitmap or doing filesystem I/O.
func (r *TorrentRepo) CheckResumeAdmission(ctx context.Context, m *StorageMapping, record string) error {
	tx, err := r.db.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkResumeFence(contextReadTx{Tx: tx, ctx: ctx}, m, record); err != nil {
		return err
	}
	return tx.Commit()
}
