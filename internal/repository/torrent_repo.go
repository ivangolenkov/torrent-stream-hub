package repository

import (
	"database/sql"
	"fmt"
	"strings"
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
	tx, err := r.db.DB().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Insert or replace torrent metadata
	_, err = tx.Exec(`
		INSERT INTO torrents (hash, name, title, data, poster, category, size, downloaded, state, error, source_uri, is_temporary, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(hash) DO UPDATE SET 
			name=excluded.name, 
			title=CASE WHEN excluded.title != '' THEN excluded.title ELSE torrents.title END,
			data=CASE WHEN excluded.data != '' THEN excluded.data ELSE torrents.data END,
			poster=CASE WHEN excluded.poster != '' THEN excluded.poster ELSE torrents.poster END,
			category=CASE WHEN excluded.category != '' THEN excluded.category ELSE torrents.category END,
			size=excluded.size, 
			downloaded=MAX(torrents.downloaded, excluded.downloaded), 
			state=excluded.state, 
			error=excluded.error,
			source_uri=CASE WHEN excluded.source_uri != '' THEN excluded.source_uri ELSE torrents.source_uri END,
			updated_at=CURRENT_TIMESTAMP
	`, t.Hash, t.Name, t.Title, t.Data, t.Poster, t.Category, t.Size, t.Downloaded, string(t.State), string(t.Error), t.SourceURI, policy != nil && *policy)
	if err != nil {
		return fmt.Errorf("failed to save torrent: %w", err)
	}
	if policy != nil && !*policy {
		if _, err := tx.Exec(`UPDATE torrents SET is_temporary = 0 WHERE hash = ?`, t.Hash); err != nil {
			return err
		}
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
				downloaded=MAX(files.downloaded, excluded.downloaded),
				priority=excluded.priority
		`, strings.Join(placeholders, ", "))

		if _, err := tx.Exec(query, values...); err != nil {
			return fmt.Errorf("failed to save files: %w", err)
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

	return torrents, nil
}

func (r *TorrentRepo) loadFiles(t *models.Torrent) error {
	rows, err := r.db.DB().Query(`SELECT "index", path, size, downloaded, priority, is_media FROM files WHERE hash = ? ORDER BY "index" ASC`, t.Hash)
	if err != nil {
		return err
	}
	defer rows.Close()

	t.Files = nil
	for rows.Next() {
		f := &models.File{}
		if err := rows.Scan(&f.Index, &f.Path, &f.Size, &f.Downloaded, &f.Priority, &f.IsMedia); err != nil {
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
		SELECT f.hash, f."index", f.path, f.size, f.downloaded, f.priority, f.is_media
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
		if err := rows.Scan(&hash, &f.Index, &f.Path, &f.Size, &f.Downloaded, &f.Priority, &f.IsMedia); err != nil {
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
	// Explicit deletes also cover legacy history without a foreign key and
	// connections created by older SQLite pool configurations.
	for _, query := range []string{
		`DELETE FROM viewed_history WHERE hash = ?`,
		`DELETE FROM files WHERE hash = ?`,
		`DELETE FROM torrents WHERE hash = ?`,
	} {
		if _, err := tx.Exec(query, hash); err != nil {
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
	_, err := r.db.DB().Exec(`UPDATE files SET priority = ? WHERE hash = ? AND "index" = ?`, priority, hash, fileIndex)
	return err
}

func (r *TorrentRepo) UpdateTorrentFilesPriority(hash string, priority models.FilePriority) error {
	_, err := r.db.DB().Exec(`UPDATE files SET priority = ? WHERE hash = ?`, priority, hash)
	return err
}
