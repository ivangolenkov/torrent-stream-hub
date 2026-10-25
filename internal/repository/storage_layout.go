package repository

import (
	"context"
	"torrent-stream-hub/internal/payload"
)

// SetStorageLayout atomically changes every alias of an opaque storage owner.
// Empty recordID is allowed only for an unpublished reservation. Filesystem
// checks/reservations belong to the caller, outside this short SQL transaction.
func (r *TorrentRepo) SetStorageLayout(ctx context.Context, before *StorageMapping, recordID string, next payload.Layout) error {
	if err := next.Validate(); err != nil {
		return err
	}
	tx, err := r.db.BeginWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := setStorageLayoutTx(tx, before, recordID, next); err != nil {
		return err
	}
	return tx.Commit()
}

func setStorageLayoutTx(tx *writeTx, before *StorageMapping, recordID string, next payload.Layout) error {
	if err := next.Validate(); err != nil {
		return err
	}
	var err error
	if err := checkStorageClaims(tx, before.StorageID, next.Root); err != nil {
		return err
	}
	var n int
	if recordID != "" {
		err = tx.QueryRow(`SELECT count(*) FROM torrent_intents WHERE hash=? AND record_id=?`, before.Hash, recordID).Scan(&n)
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrStateConflict
		}
	} else {
		err = tx.QueryRow(`SELECT count(*) FROM torrents t JOIN torrent_storage s ON t.hash=s.hash WHERE s.storage_id=?`, before.StorageID).Scan(&n)
		if err != nil {
			return err
		}
		if n != 0 {
			return ErrStateConflict
		}
	}
	err = tx.QueryRow(`SELECT count(*) FROM torrent_storage WHERE hash=? AND storage_id=? AND revision=? AND root=? AND prefix=? AND version=?`, before.Hash, before.StorageID, before.Revision, before.Root, before.Prefix, before.Version).Scan(&n)
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrStateConflict
	}
	rows, err := tx.Query(`SELECT storage_id,root,revision,prefix,version FROM torrent_storage`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, root, prefix string
		var revision int64
		var version int
		if err = rows.Scan(&id, &root, &revision, &prefix, &version); err != nil {
			break
		}
		if id == before.StorageID {
			if root != before.Root || revision != before.Revision || prefix != before.Prefix || version != before.Version {
				err = ErrStateConflict
				break
			}
		} else if root != "" && payload.Overlap(payload.CollisionKey(root), payload.CollisionKey(next.Root)) {
			err = ErrStateConflict
			break
		}
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if readErr != nil {
		return readErr
	}
	_, err = tx.Exec(`UPDATE torrent_storage SET root=?,version=?,prefix=?,state='ready',error='' WHERE storage_id=? AND revision=?`, next.Root, next.Version, next.Prefix, before.StorageID, before.Revision)
	if err != nil {
		return err
	}
	return nil
}
