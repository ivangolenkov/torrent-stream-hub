-- Preserve existing keys while extending the finite safety producer set. This
-- also upgrades development databases which already applied phase 39's 0009.
CREATE TABLE torrent_activation_blocks_v10 (
    record_id TEXT NOT NULL REFERENCES torrent_intents(record_id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK(kind IN ('verification','missing_files','legacy_error','metadata_persistence','invalid_selection')),
    reason TEXT NOT NULL,
    PRIMARY KEY(record_id,kind)
);
INSERT INTO torrent_activation_blocks_v10 SELECT record_id,kind,reason FROM torrent_activation_blocks;
DROP TABLE torrent_activation_blocks;
ALTER TABLE torrent_activation_blocks_v10 RENAME TO torrent_activation_blocks;
