package repository

import (
	"database/sql"
	"fmt"
	"sync"
	"torrent-stream-hub/internal/models"
)

type TorrentRepo struct {
	db *SQLiteDB
	// Lifecycle serializes application mutations and sync snapshots, not network I/O.
	Lifecycle sync.Mutex
}

func NewTorrentRepo(db *SQLiteDB) *TorrentRepo {
	return &TorrentRepo{db: db}
}

func (r *TorrentRepo) SaveTorrent(t *models.Torrent) error {
	return r.saveTorrent(t, nil)
}

// SaveTorrentWithPolicy initializes policy on insert; only explicit permanence
// may change it afterwards. Ordinary statistics saves never change policy.
func (r *TorrentRepo) SaveTorrentWithPolicy(t *models.Torrent, temporary bool) error {
	return r.saveTorrent(t, &temporary)
}

func (r *TorrentRepo) saveTorrent(t *models.Torrent, policy *bool) error {
	if t == nil || t.Hash == "" {
		return fmt.Errorf("invalid torrent snapshot")
	}
	indices := make(map[int]bool, len(t.Files))
	for _, f := range t.Files {
		if f == nil || f.Index < 0 || f.Size < 0 || !models.ValidFilePriority(f.Priority) || indices[f.Index] {
			return fmt.Errorf("invalid file snapshot")
		}
		indices[f.Index] = true
	}
	tx, err := r.db.DB().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previousObservation int64
	err = tx.QueryRow(`SELECT progress_observed_at FROM torrents WHERE hash=?`, t.Hash).Scan(&previousObservation)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if t.ProgressSource == "verified" && t.ProgressObservedAt <= 0 {
		return fmt.Errorf("verified snapshot requires observation time")
	}
	writeProgress := t.ProgressSource == "verified" && t.ProgressObservedAt > previousObservation ||
		(t.ProgressSource == "" || t.ProgressSource == "legacy") && previousObservation == 0
	writeMetadata := t.MetadataKnown || t.ProgressSource == "" || t.ProgressSource == "legacy"
	initialSource := t.ProgressSource
	if initialSource == "" {
		initialSource = "legacy"
	}
	if initialSource != "verified" && initialSource != "legacy" && initialSource != "last_known" {
		initialSource = "unknown"
	}

	// Insert or replace torrent metadata
	_, err = tx.Exec(`
		INSERT INTO torrents (hash, name, title, data, poster, category, size, downloaded, state, error, source_uri, is_temporary, progress_source, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(hash) DO UPDATE SET 
			name=CASE WHEN ? OR torrents.name='' THEN excluded.name ELSE torrents.name END,
			title=CASE WHEN excluded.title != '' THEN excluded.title ELSE torrents.title END,
			data=CASE WHEN excluded.data != '' THEN excluded.data ELSE torrents.data END,
			poster=CASE WHEN excluded.poster != '' THEN excluded.poster ELSE torrents.poster END,
			category=CASE WHEN excluded.category != '' THEN excluded.category ELSE torrents.category END,
			size=CASE WHEN ? THEN excluded.size ELSE torrents.size END,
			downloaded=CASE WHEN ? THEN excluded.downloaded ELSE torrents.downloaded END,
			state=CASE WHEN torrents.deletion_mode != '' THEN torrents.state ELSE excluded.state END,
			error=CASE WHEN torrents.deletion_mode != '' THEN torrents.error ELSE excluded.error END,
			source_uri=CASE WHEN excluded.source_uri != '' THEN excluded.source_uri ELSE torrents.source_uri END,
			updated_at=CURRENT_TIMESTAMP
	`, t.Hash, t.Name, t.Title, t.Data, t.Poster, t.Category, t.Size, max(int64(0), t.Downloaded), string(t.State), string(t.Error), t.SourceURI, policy != nil && *policy, initialSource, writeMetadata, writeMetadata, writeProgress)
	if err != nil {
		return fmt.Errorf("failed to save torrent: %w", err)
	}
	if policy != nil && !*policy {
		if _, err := tx.Exec(`UPDATE torrents SET is_temporary = 0 WHERE hash = ?`, t.Hash); err != nil {
			return err
		}
	}
	if writeProgress {
		source := "legacy"
		if t.ProgressSource == "verified" {
			source = "verified"
		}
		if _, err := tx.Exec(`UPDATE torrents SET progress_source=?, progress_observed_at=?, legacy_downloaded=CASE WHEN ?='legacy' THEN MAX(legacy_downloaded,?) ELSE legacy_downloaded END WHERE hash=?`, source, t.ProgressObservedAt, source, max(int64(0), t.Downloaded), t.Hash); err != nil {
			return err
		}
	}
	if t.MetadataKnown {
		if _, err := tx.Exec(`UPDATE torrents SET piece_length=?,num_pieces=?,metadata_known=1 WHERE hash=?`, t.PieceLength, t.NumPieces, t.Hash); err != nil {
			return err
		}
	}

	// Insert or update files
	if len(t.Files) > 0 {
		stmt, err := tx.Prepare(`
			INSERT INTO files (hash, "index", path, size, downloaded, priority, is_media, file_offset, offset_known, legacy_downloaded)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(hash, "index") DO UPDATE SET 
				downloaded=CASE WHEN ? THEN excluded.downloaded ELSE files.downloaded END,
				file_offset=CASE WHEN excluded.offset_known THEN excluded.file_offset ELSE files.file_offset END,
				offset_known=MAX(files.offset_known, excluded.offset_known),
				legacy_downloaded=MAX(files.legacy_downloaded,excluded.legacy_downloaded)
		`)
		if err != nil {
			return fmt.Errorf("failed to prepare files: %w", err)
		}
		defer stmt.Close()
		for _, f := range t.Files {
			legacy := int64(0)
			if t.ProgressSource == "" || t.ProgressSource == "legacy" {
				legacy = max(int64(0), f.Downloaded)
			}
			if _, err := stmt.Exec(t.Hash, f.Index, f.Path, f.Size, max(int64(0), f.Downloaded), f.Priority, f.IsMedia, f.Offset, f.OffsetKnown, legacy, writeProgress); err != nil {
				return fmt.Errorf("failed to save file %d: %w", f.Index, err)
			}
		}
	}

	return tx.Commit()
}

func (r *TorrentRepo) GetTorrent(hash string) (*models.Torrent, error) {
	row := r.db.DB().QueryRow(`SELECT hash, name, title, data, poster, category, size, downloaded, state, error, source_uri, is_temporary, COALESCE((SELECT state FROM torrent_storage WHERE hash=torrents.hash),'legacy'), COALESCE((SELECT error FROM torrent_storage WHERE hash=torrents.hash),'') FROM torrents WHERE hash = ?`, hash)

	t := &models.Torrent{}
	var stateStr, errorStr string

	if err := row.Scan(&t.Hash, &t.Name, &t.Title, &t.Data, &t.Poster, &t.Category, &t.Size, &t.Downloaded, &stateStr, &errorStr, &t.SourceURI, &t.IsTemporary, &t.StorageState, &t.StorageError); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // not found
		}
		return nil, err
	}

	t.State = models.TorrentState(stateStr)
	t.Error = models.ErrorReason(errorStr)

	if err := r.loadFiles(t); err != nil {
		return nil, err
	}
	block, err := r.DownloadBlock(hash)
	if err != nil {
		return nil, err
	}
	t.DownloadBlock = block
	if err := r.loadDeletionIntents(map[string]*models.Torrent{hash: t}); err != nil {
		return nil, err
	}
	if err := r.loadProgressObservations(map[string]*models.Torrent{hash: t}); err != nil {
		return nil, err
	}

	return t, nil
}

func (r *TorrentRepo) GetAllTorrents() ([]*models.Torrent, error) {
	rows, err := r.db.DB().Query(`SELECT hash, name, title, data, poster, category, size, downloaded, state, error, source_uri, is_temporary, COALESCE((SELECT state FROM torrent_storage WHERE hash=torrents.hash),'legacy'), COALESCE((SELECT error FROM torrent_storage WHERE hash=torrents.hash),'') FROM torrents ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	torrents := make([]*models.Torrent, 0)
	byHash := make(map[string]*models.Torrent)
	for rows.Next() {
		t := &models.Torrent{}
		var stateStr, errorStr string
		if err := rows.Scan(&t.Hash, &t.Name, &t.Title, &t.Data, &t.Poster, &t.Category, &t.Size, &t.Downloaded, &stateStr, &errorStr, &t.SourceURI, &t.IsTemporary, &t.StorageState, &t.StorageError); err != nil {
			return nil, err
		}
		t.State = models.TorrentState(stateStr)
		t.Error = models.ErrorReason(errorStr)
		byHash[t.Hash] = t
		torrents = append(torrents, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := r.loadFilesForTorrents(byHash); err != nil {
		return nil, err
	}
	if err := r.loadDownloadBlocks(byHash); err != nil {
		return nil, err
	}
	if err := r.loadDeletionIntents(byHash); err != nil {
		return nil, err
	}
	if err := r.loadProgressObservations(byHash); err != nil {
		return nil, err
	}

	return torrents, nil
}

func (r *TorrentRepo) loadFiles(t *models.Torrent) error {
	rows, err := r.db.DB().Query(`SELECT "index", path, size, downloaded, priority, is_media, file_offset, offset_known, legacy_downloaded FROM files WHERE hash = ? ORDER BY "index" ASC`, t.Hash)
	if err != nil {
		return err
	}
	defer rows.Close()

	t.Files = nil
	for rows.Next() {
		f := &models.File{}
		if err := rows.Scan(&f.Index, &f.Path, &f.Size, &f.Downloaded, &f.Priority, &f.IsMedia, &f.Offset, &f.OffsetKnown, &f.LegacyDownloaded); err != nil {
			return err
		}
		t.Files = append(t.Files, f)
	}
	return rows.Err()
}

func (r *TorrentRepo) loadFilesForTorrents(torrents map[string]*models.Torrent) error {
	if len(torrents) == 0 {
		return nil
	}

	rows, err := r.db.DB().Query(`
		SELECT f.hash, f."index", f.path, f.size, f.downloaded, f.priority, f.is_media, f.file_offset, f.offset_known, f.legacy_downloaded
		FROM files f
		JOIN torrents t ON t.hash = f.hash
		ORDER BY f.hash ASC, f."index" ASC
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var hash string
		f := &models.File{}
		if err := rows.Scan(&hash, &f.Index, &f.Path, &f.Size, &f.Downloaded, &f.Priority, &f.IsMedia, &f.Offset, &f.OffsetKnown, &f.LegacyDownloaded); err != nil {
			return err
		}
		if t := torrents[hash]; t != nil {
			t.Files = append(t.Files, f)
		}
	}
	return rows.Err()
}

func (r *TorrentRepo) DeleteTorrent(hash string) error {
	tx, err := r.db.DB().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var mode, root string
	err = tx.QueryRow(`SELECT deletion_mode,COALESCE((SELECT root FROM torrent_storage WHERE hash=torrents.hash),'') FROM torrents WHERE hash=?`, hash).Scan(&mode, &root)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	// Explicit deletes also cover legacy history without a foreign key and
	// connections created by older SQLite pool configurations.
	for _, query := range []string{
		`DELETE FROM torrent_download_blocks WHERE hash = ?`,
		`DELETE FROM viewed_history WHERE hash = ?`,
		`DELETE FROM files WHERE hash = ?`,
		`DELETE FROM torrents WHERE hash = ?`,
	} {
		if _, err := tx.Exec(query, hash); err != nil {
			return err
		}
	}
	// Keep-data deletion intentionally retains namespace reservations: re-add
	// must reopen the same isolated root, not guess an occupied new namespace.
	if mode == "payload" {
		if root == "" {
			_, err = tx.Exec(`DELETE FROM torrent_storage WHERE hash=?`, hash)
		} else {
			_, err = tx.Exec(`DELETE FROM torrent_storage WHERE root=? AND NOT EXISTS (SELECT 1 FROM torrents t JOIN torrent_storage s ON s.hash=t.hash WHERE s.root=?)`, root, root)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *TorrentRepo) MakePermanent(hash string) (bool, error) {
	res, err := r.db.DB().Exec(`UPDATE torrents SET is_temporary = 0 WHERE hash = ?`, hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (r *TorrentRepo) TemporaryQueue() ([]string, error) {
	rows, err := r.db.DB().Query(`SELECT t.hash FROM torrents t JOIN torrent_order q ON q.hash = t.hash WHERE t.is_temporary = 1 ORDER BY q.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		hashes = append(hashes, hash)
	}
	return hashes, rows.Err()
}

func (r *TorrentRepo) UpdateFilePriority(hash string, fileIndex int, priority models.FilePriority) error {
	if fileIndex < 0 || !models.ValidFilePriority(priority) {
		return ErrInvalidPriority
	}
	return r.updatePriority(hash, &fileIndex, priority)
}

func (r *TorrentRepo) UpdateTorrentFilesPriority(hash string, priority models.FilePriority) error {
	if !models.ValidFilePriority(priority) {
		return ErrInvalidPriority
	}
	return r.updatePriority(hash, nil, priority)
}

func (r *TorrentRepo) updatePriority(hash string, index *int, priority models.FilePriority) error {
	tx, err := r.db.DB().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	query := `UPDATE files SET priority = ? WHERE hash = ?`
	args := []any{priority, hash}
	if index != nil {
		query += ` AND "index" = ?`
		args = append(args, *index)
	}
	res, err := tx.Exec(query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var exists, files int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM torrents WHERE hash=?`, hash).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrTorrentNotFound
		}
		if err := tx.QueryRow(`SELECT COUNT(*) FROM files WHERE hash=?`, hash).Scan(&files); err != nil {
			return err
		}
		if files == 0 {
			return ErrMetadataPending
		}
		return ErrFileNotFound
	}
	return tx.Commit()
}
