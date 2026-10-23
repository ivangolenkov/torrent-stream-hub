package repository

import (
	"database/sql"
	"fmt"
	"math"
	"path"

	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
)

func requireFence(tx sqlWriter, f models.RuntimeFence) error {
	if f.RecordID == "" || f.Attachment == "" || f.Revision < 1 {
		return ErrStateConflict
	}
	var exists int
	err := tx.QueryRow(`SELECT 1 FROM torrent_intents i JOIN torrents t ON t.hash=i.hash
        WHERE i.hash=? AND i.record_id=? AND i.revision=? AND i.runtime_attachment=? AND t.deletion_mode=''
        AND NOT EXISTS(SELECT 1 FROM torrent_storage s WHERE s.hash=i.hash AND s.state='moving')`, f.Hash, f.RecordID, f.Revision, f.Attachment).Scan(&exists)
	if err == sql.ErrNoRows {
		return ErrStateConflict
	}
	return err
}

// SaveCatalog accepts only current runtime metainfo. It does not create parent
// records or accept selection/progress/user metadata; existing priorities survive.
func (r *TorrentRepo) SaveCatalog(f models.RuntimeFence, c models.TorrentCatalog) error {
	if err := validateCatalog(c); err != nil {
		return err
	}
	tx, err := r.db.BeginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireFence(tx, f); err != nil {
		return err
	}
	if err := saveCatalogTx(tx, f.Hash, c); err != nil {
		return err
	}
	return tx.Commit()
}

func validateCatalog(c models.TorrentCatalog) error {
	if c.Size < 0 || c.PieceLength <= 0 || c.NumPieces < 0 {
		return fmt.Errorf("invalid catalog metadata")
	}
	var size int64
	for i, file := range c.Files {
		if file.Index != i || file.Size < 0 || file.Offset != size || file.Size > math.MaxInt64-size || payload.ValidateRelative(file.Path) != nil {
			return fmt.Errorf("invalid file catalog")
		}
		size += file.Size
	}
	if size != c.Size {
		return fmt.Errorf("inconsistent catalog size")
	}
	return nil
}

func saveCatalogTx(tx sqlWriter, hash string, c models.TorrentCatalog) error {
	var known bool
	var oldSize, oldPiece int64
	var oldCount int
	if err := tx.QueryRow(`SELECT metadata_known,size,piece_length,num_pieces FROM torrents WHERE hash=?`, hash).Scan(&known, &oldSize, &oldPiece, &oldCount); err != nil {
		return err
	}
	if known && (oldSize != c.Size || oldPiece != c.PieceLength || oldCount != c.NumPieces) {
		return ErrStateConflict
	}
	// A metainfo generation cannot replace an already known file identity.
	rows, err := tx.Query(`SELECT "index",path,size,file_offset,offset_known FROM files WHERE hash=?`, hash)
	if err != nil {
		return err
	}
	for rows.Next() {
		var index int
		var storedPath string
		var bytes, offset int64
		var offsetKnown bool
		if err := rows.Scan(&index, &storedPath, &bytes, &offset, &offsetKnown); err != nil {
			rows.Close()
			return err
		}
		if index < 0 || index >= len(c.Files) {
			rows.Close()
			return ErrStateConflict
		}
		file := c.Files[index]
		// Legacy snapshots sometimes stored the root-prefixed metainfo path.
		// Normalize only this exact representation, never probe filesystem paths.
		legacyPath := !known && storedPath == path.Join(c.Name, file.Path)
		if storedPath != file.Path && !legacyPath || bytes != file.Size || offsetKnown && offset != file.Offset {
			rows.Close()
			return ErrStateConflict
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if _, err := tx.Exec(`UPDATE torrents SET name=?,size=?,piece_length=?,num_pieces=?,metadata_known=1 WHERE hash=?`, c.Name, c.Size, c.PieceLength, c.NumPieces, hash); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO files(hash,"index",path,size,downloaded,priority,is_media,file_offset,offset_known)
        VALUES(?,?,?,?,0,0,?,?,1) ON CONFLICT(hash,"index") DO UPDATE SET path=excluded.path,file_offset=excluded.file_offset,offset_known=1,is_media=excluded.is_media`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, file := range c.Files {
		if _, err := stmt.Exec(hash, file.Index, file.Path, file.Size, file.IsMedia, file.Offset); err != nil {
			return err
		}
	}
	return nil
}

// CreateTorrent is insert-only and commits parent, creation intent and catalog
// together. It cannot overwrite an existing record or initialize observed bytes.
func (r *TorrentRepo) CreateTorrent(n models.NewTorrentRecord) error {
	if n.Hash == "" || !n.Run.Valid() {
		return fmt.Errorf("invalid new torrent record")
	}
	if n.Catalog != nil {
		if err := validateCatalog(*n.Catalog); err != nil {
			return err
		}
	}
	tx, err := r.db.BeginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	state := models.StateQueued
	if n.Run == models.RunPaused {
		state = models.StatePaused
	}
	if _, err := tx.Exec(`INSERT INTO torrents(hash,name,size,state,error,source_uri,is_temporary,title,data,poster,category,progress_source)
        VALUES(?,?,0,?,'',?,?,?,?,?,?,'unknown')`, n.Hash, n.Name, state, n.SourceURI, n.Temporary, n.Metadata.Title, n.Metadata.Data, n.Metadata.Poster, n.Metadata.Category); err != nil {
		return err
	}
	if n.Catalog != nil {
		if err := saveCatalogTx(tx, n.Hash, *n.Catalog); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpdateUserMetadata preserves the historical nonempty-field patch contract.
// The explicit column list cannot write runtime state, bytes or catalog fields.
func (r *TorrentRepo) UpdateUserMetadata(v models.IntentVersion, m models.UserMetadata) (models.IntentVersion, error) {
	tx, err := r.db.BeginWrite()
	if err != nil {
		return v, err
	}
	defer tx.Rollback()
	if err := advanceIntent(tx, v); err != nil {
		return v, err
	}
	if _, err := tx.Exec(`UPDATE torrents SET title=CASE WHEN ?!='' THEN ? ELSE title END,
        data=CASE WHEN ?!='' THEN ? ELSE data END,poster=CASE WHEN ?!='' THEN ? ELSE poster END,
        category=CASE WHEN ?!='' THEN ? ELSE category END WHERE hash=?`, m.Title, m.Title, m.Data, m.Data, m.Poster, m.Poster, m.Category, m.Category, v.Hash); err != nil {
		return v, err
	}
	if err := tx.Commit(); err != nil {
		return v, err
	}
	v.Revision++
	return v, nil
}

func advanceIntent(tx sqlWriter, v models.IntentVersion) error {
	if v.Revision < 1 || v.Revision == math.MaxInt64 || v.RecordID == "" {
		return ErrStateConflict
	}
	res, err := tx.Exec(`UPDATE torrent_intents SET revision=revision+1 WHERE hash=? AND record_id=? AND revision=?
        AND EXISTS(SELECT 1 FROM torrents t WHERE t.hash=torrent_intents.hash AND t.deletion_mode='')`, v.Hash, v.RecordID, v.Revision)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrStateConflict
	}
	return nil
}

func intentVersionTx(tx sqlWriter, hash string) (models.IntentVersion, error) {
	v := models.IntentVersion{Hash: hash}
	err := tx.QueryRow(`SELECT record_id,revision FROM torrent_intents WHERE hash=?`, hash).Scan(&v.RecordID, &v.Revision)
	return v, err
}
