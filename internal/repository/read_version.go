package repository

import "context"

func (r *TorrentRepo) ReadVersion(hashes ...string) (int64, error) {
	return r.ReadVersionContext(context.Background(), hashes...)
}

func (r *TorrentRepo) ReadVersionContext(ctx context.Context, hashes ...string) (int64, error) {
	var version int64
	if len(hashes) > 0 {
		err := r.db.DB().QueryRowContext(ctx, `SELECT COALESCE((SELECT read_version FROM torrents WHERE hash=?),0)`, hashes[0]).Scan(&version)
		return version, err
	}
	err := r.db.DB().QueryRowContext(ctx, `SELECT version FROM read_model_version WHERE singleton=1`).Scan(&version)
	return version, err
}
