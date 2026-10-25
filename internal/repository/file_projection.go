package repository

import (
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
)

// Materialize historical evidence in the same WAL snapshot as mapping and
// catalog. No filesystem reads, and no persisted byte count becomes verified.
func loadFileObservations(tx sqlReader, torrents map[string]*models.Torrent) error {
	byFile := make(map[string]map[int]*models.File, len(torrents))
	for hash, t := range torrents {
		t.LastKnownBytes, t.LastKnownAtMS, t.LastKnownSource = t.Downloaded, t.ProgressObservedAt/1e6, t.ProgressSource
		byFile[hash] = make(map[int]*models.File, len(t.Files))
		for _, f := range t.Files {
			f.Status = &models.FileStatus{Presence: "unknown", Verification: "unknown", ReadReason: "runtime_not_attached", EffectivePriority: "none",
				LastKnownBytes: f.Downloaded, LastKnownAtMS: t.ProgressObservedAt / 1e6, LastKnownSource: t.ProgressSource}
			if s := t.Storage; s != nil && f.MetadataPath != "" {
				layout := payload.Layout{Version: s.LayoutVersion, Root: s.Root, Prefix: s.Prefix}
				if path, err := layout.Path(f.MetadataPath); err == nil {
					f.Status.StoragePath = path
				}
			}
			byFile[hash][f.Index] = f
		}
	}
	query := `SELECT e.hash,e.file_index,e.presence,e.reason,e.observed_at_ms
 FROM file_inventory e JOIN torrent_intents i ON i.hash=e.hash AND i.record_id=e.record_id
 JOIN torrent_storage s ON s.hash=e.hash AND s.revision=e.mapping_revision
 JOIN files f ON f.hash=e.hash AND f."index"=e.file_index AND f.path=e.metadata_path AND f.size=e.size`
	var args []any
	if len(torrents) == 1 {
		for hash := range torrents {
			query += ` WHERE e.hash=?`
			args = append(args, hash)
		}
	}
	rows, err := tx.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var hash, presence, reason string
		var index int
		var observed int64
		if err := rows.Scan(&hash, &index, &presence, &reason, &observed); err != nil {
			return err
		}
		if f := byFile[hash][index]; f != nil {
			f.Status.Presence, f.Status.Reason, f.Status.ObservedAtMS = presence, reason, observed
		}
	}
	return rows.Err()
}
