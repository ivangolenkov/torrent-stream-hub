-- Old downloaded is an observation, neither verified in this opening nor
-- cumulative network traffic. Preserve it without granting readiness.
ALTER TABLE torrents ADD COLUMN legacy_downloaded INTEGER NOT NULL DEFAULT 0;
ALTER TABLE torrents ADD COLUMN progress_observed_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE torrents ADD COLUMN progress_source TEXT NOT NULL DEFAULT 'legacy';
ALTER TABLE torrents ADD COLUMN piece_length INTEGER NOT NULL DEFAULT 0;
ALTER TABLE torrents ADD COLUMN num_pieces INTEGER NOT NULL DEFAULT 0;
ALTER TABLE torrents ADD COLUMN metadata_known INTEGER NOT NULL DEFAULT 0;
UPDATE torrents SET legacy_downloaded=downloaded;
ALTER TABLE files ADD COLUMN legacy_downloaded INTEGER NOT NULL DEFAULT 0;
ALTER TABLE files ADD COLUMN file_offset INTEGER NOT NULL DEFAULT 0;
ALTER TABLE files ADD COLUMN offset_known INTEGER NOT NULL DEFAULT 0;
UPDATE files SET legacy_downloaded=downloaded;
