ALTER TABLE torrents ADD COLUMN is_temporary INTEGER NOT NULL DEFAULT 0 CHECK (is_temporary IN (0, 1));
ALTER TABLE torrents ADD COLUMN cleanup_paths TEXT NOT NULL DEFAULT '';
ALTER TABLE torrents ADD COLUMN cleanup_ready INTEGER NOT NULL DEFAULT 0 CHECK (cleanup_ready IN (0, 1));

-- A separate sequence avoids rebuilding torrents (and its foreign keys).
CREATE TABLE torrent_order (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    hash TEXT NOT NULL UNIQUE
);
INSERT INTO torrent_order (hash) SELECT hash FROM torrents ORDER BY created_at, rowid;
CREATE TRIGGER torrent_order_insert AFTER INSERT ON torrents BEGIN
    INSERT INTO torrent_order (hash) VALUES (NEW.hash);
END;
CREATE TRIGGER torrent_order_delete AFTER DELETE ON torrents BEGIN
    DELETE FROM torrent_order WHERE hash = OLD.hash;
END;
CREATE INDEX torrents_temporary ON torrents(is_temporary);
