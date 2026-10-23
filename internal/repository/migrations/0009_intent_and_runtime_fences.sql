-- Each logical record has an identity independent of infohash. Re-add must not
-- inherit capabilities issued to a deleted record or an earlier attachment.
CREATE TABLE torrent_intents (
    hash TEXT PRIMARY KEY REFERENCES torrents(hash) ON DELETE CASCADE,
    record_id TEXT NOT NULL UNIQUE,
    run_intent TEXT NOT NULL CHECK(run_intent IN ('paused','enabled')),
    revision INTEGER NOT NULL DEFAULT 1 CHECK(revision > 0),
    runtime_attachment TEXT NOT NULL DEFAULT '',
    observation_sequence INTEGER NOT NULL DEFAULT 0 CHECK(observation_sequence >= 0),
    applied_revision INTEGER NOT NULL DEFAULT 0 CHECK(applied_revision >= 0),
    observed_state TEXT NOT NULL DEFAULT '',
    observed_error TEXT NOT NULL DEFAULT ''
);

INSERT INTO torrent_intents(hash,record_id,run_intent,observed_state,observed_error)
SELECT hash,lower(hex(randomblob(16))),
    CASE WHEN state IN ('Queued','Downloading','Seeding','Streaming','DiskFull')
         THEN 'enabled' ELSE 'paused' END,
    state,COALESCE(error,'') FROM torrents;

CREATE TABLE torrent_activation_blocks (
    record_id TEXT NOT NULL REFERENCES torrent_intents(record_id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK(kind IN ('verification','missing_files','legacy_error')),
    reason TEXT NOT NULL,
    PRIMARY KEY(record_id,kind)
);
INSERT INTO torrent_activation_blocks(record_id,kind,reason)
SELECT i.record_id,CASE WHEN t.state='MissingFiles' THEN 'missing_files' ELSE 'legacy_error' END,
    CASE WHEN COALESCE(t.error,'')='' THEN 'Legacy activation requires explicit retry' ELSE t.error END
FROM torrents t JOIN torrent_intents i ON i.hash=t.hash WHERE t.state IN ('Error','MissingFiles');

-- Repair legacy DiskFull rows without erasing a more precise existing reason.
INSERT INTO torrent_download_blocks(hash,reason)
SELECT hash,CASE WHEN COALESCE(error,'')='' THEN 'Legacy DiskFull: explicit Resume required' ELSE error END
FROM torrents WHERE state='DiskFull'
ON CONFLICT(hash) DO UPDATE SET reason=CASE WHEN torrent_download_blocks.reason=''
    THEN excluded.reason ELSE torrent_download_blocks.reason END;

-- Creation-only compatibility bridge while command paths are migrated. It does
-- not observe UPDATEs: a runtime snapshot must never change saved run intent.
CREATE TRIGGER torrents_initialize_intent AFTER INSERT ON torrents
BEGIN
    INSERT INTO torrent_intents(hash,record_id,run_intent)
    VALUES(NEW.hash,lower(hex(randomblob(16))),
        CASE WHEN NEW.state IN ('Queued','Downloading','Seeding','Streaming','DiskFull')
             THEN 'enabled' ELSE 'paused' END);
END;
