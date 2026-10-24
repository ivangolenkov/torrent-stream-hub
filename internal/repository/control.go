package repository

import (
	"torrent-stream-hub/internal/models"
)

// Called in the same read transaction as catalog/selection/workflows. Persisted
// applied_revision is historical, not acknowledgment by the current runtime.
func loadControls(tx sqlReader, torrents map[string]*models.Torrent) error {
	if len(torrents) == 0 {
		return nil
	}
	query := `SELECT hash,record_id,revision,run_intent FROM torrent_intents`
	var args []any
	if len(torrents) == 1 {
		for hash := range torrents {
			query += ` WHERE hash=?`
			args = append(args, hash)
		}
	}
	rows, err := tx.Query(query, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var hash string
		c := &models.TorrentControl{ReconcilePending: true, ReconcileReason: "runtime_not_attached", ActivationBlocks: []models.ActivationBlock{}}
		if err := rows.Scan(&hash, &c.RecordID, &c.DesiredRevision, &c.DesiredRun); err != nil {
			rows.Close()
			return err
		}
		if t := torrents[hash]; t != nil {
			t.Control = c
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, t := range torrents {
		if t.Control == nil {
			return ErrStateConflict
		}
	}
	rows, err = tx.Query(`SELECT i.hash,b.kind,b.reason FROM torrent_activation_blocks b JOIN torrent_intents i ON i.record_id=b.record_id ORDER BY b.kind`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var hash string
		var b models.ActivationBlock
		if err := rows.Scan(&hash, &b.Kind, &b.Reason); err != nil {
			return err
		}
		if t := torrents[hash]; t != nil {
			t.Control.ActivationBlocks = append(t.Control.ActivationBlocks, b)
		}
	}
	return rows.Err()
}
