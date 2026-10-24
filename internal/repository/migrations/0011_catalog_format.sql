-- Do not infer a v1 layout from legacy offsets. Matching metainfo supplies the
-- format on the next catalog capture; this column never grants byte readiness.
ALTER TABLE torrents ADD COLUMN metadata_format TEXT NOT NULL DEFAULT ''
    CHECK(metadata_format IN ('','v1','v2','hybrid'));
