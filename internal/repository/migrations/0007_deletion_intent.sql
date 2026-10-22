ALTER TABLE torrents ADD COLUMN deletion_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE torrents ADD COLUMN deletion_error TEXT NOT NULL DEFAULT '';
UPDATE torrents SET deletion_mode='payload' WHERE cleanup_paths != '';
