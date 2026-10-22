-- Mapping is independent of runtime snapshots. New reservations can precede
-- torrent insertion; old records are explicitly legacy, never guessed by stat.
CREATE TABLE torrent_storage (
    hash TEXT PRIMARY KEY,
    version INTEGER NOT NULL DEFAULT 0,
    root TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'legacy',
    manifest TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT ''
);
INSERT INTO torrent_storage(hash) SELECT hash FROM torrents;
