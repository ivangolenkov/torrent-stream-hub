ALTER TABLE files ADD COLUMN metadata_path TEXT NOT NULL DEFAULT '';
ALTER TABLE storage_recovery ADD COLUMN location_free_bytes INTEGER;
