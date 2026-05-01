package repository

import (
	"database/sql"
	"fmt"
	"strings"
	"torrent-stream-hub/internal/models"
)

type TorrentRepo struct {
	db *SQLiteDB
}

func NewTorrentRepo(db *SQLiteDB) *TorrentRepo {
	return &TorrentRepo{db: db}
}

func (r *TorrentRepo) SaveTorrent(t *models.Torrent) error {
	tx, err := r.db.DB().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Insert or replace torrent metadata
	_, err = tx.Exec(`
		INSERT INTO torrents (hash, name, title, data, poster, category, size, downloaded, state, error, source_uri, updated_at) 
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(hash) DO UPDATE SET 
			name=excluded.name, 
			title=CASE WHEN excluded.title != '' THEN excluded.title ELSE torrents.title END,
			data=CASE WHEN excluded.data != '' THEN excluded.data ELSE torrents.data END,
			poster=CASE WHEN excluded.poster != '' THEN excluded.poster ELSE torrents.poster END,
			category=CASE WHEN excluded.category != '' THEN excluded.category ELSE torrents.category END,
			size=excluded.size, 
			downloaded=excluded.downloaded, 
			state=excluded.state, 
			error=excluded.error,
			source_uri=CASE WHEN excluded.source_uri != '' THEN excluded.source_uri ELSE torrents.source_uri END,
			updated_at=CURRENT_TIMESTAMP
	`, t.Hash, t.Name, t.Title, t.Data, t.Poster, t.Category, t.Size, t.Downloaded, string(t.State), string(t.Error), t.SourceURI)
	if err != nil {
		return fmt.Errorf("failed to save torrent: %w", err)
	}

	// Insert or update files
	if len(t.Files) > 0 {
		var values []interface{}
		var placeholders []string

		for _, f := range t.Files {
			placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?)")
			values = append(values, t.Hash, f.Index, f.Path, f.Size, f.Downloaded, f.Priority, f.IsMedia)
		}

		query := fmt.Sprintf(`
			INSERT INTO files (hash, "index", path, size, downloaded, priority, is_media)
			VALUES %s
			ON CONFLICT(hash, "index") DO UPDATE SET 
				downloaded=excluded.downloaded,
				priority=excluded.priority
		`, strings.Join(placeholders, ", "))

		if _, err := tx.Exec(query, values...); err != nil {
			return fmt.Errorf("failed to save files: %w", err)
		}
	}

	return tx.Commit()
}

func (r *TorrentRepo) GetTorrent(hash string) (*models.Torrent, error) {
	row := r.db.DB().QueryRow(`SELECT hash, name, title, data, poster, category, size, downloaded, state, error, source_uri FROM torrents WHERE hash = ?`, hash)

	t := &models.Torrent{}
	var stateStr, errorStr string

	if err := row.Scan(&t.Hash, &t.Name, &t.Title, &t.Data, &t.Poster, &t.Category, &t.Size, &t.Downloaded, &stateStr, &errorStr, &t.SourceURI); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // not found
		}
		return nil, err
	}

	t.State = models.TorrentState(stateStr)
	t.Error = models.ErrorReason(errorStr)

	// Get files
	rows, err := r.db.DB().Query(`SELECT "index", path, size, downloaded, priority, is_media FROM files WHERE hash = ? ORDER BY "index" ASC`, hash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		f := &models.File{}
		if err := rows.Scan(&f.Index, &f.Path, &f.Size, &f.Downloaded, &f.Priority, &f.IsMedia); err != nil {
			return nil, err
		}
		t.Files = append(t.Files, f)
	}

	return t, nil
}

func (r *TorrentRepo) GetAllTorrents() ([]*models.Torrent, error) {
	rows, err := r.db.DB().Query(`SELECT hash, name, title, data, poster, category, size, downloaded, state, error, source_uri FROM torrents ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	torrents := make([]*models.Torrent, 0)
	for rows.Next() {
		t := &models.Torrent{}
		var stateStr, errorStr string
		if err := rows.Scan(&t.Hash, &t.Name, &t.Title, &t.Data, &t.Poster, &t.Category, &t.Size, &t.Downloaded, &stateStr, &errorStr, &t.SourceURI); err != nil {
			return nil, err
		}
		t.State = models.TorrentState(stateStr)
		t.Error = models.ErrorReason(errorStr)
		torrents = append(torrents, t)
	}

	return torrents, nil
}

func (r *TorrentRepo) DeleteTorrent(hash string) error {
	_, err := r.db.DB().Exec(`DELETE FROM torrents WHERE hash = ?`, hash)
	return err
}
