CREATE TABLE storage_volumes(root TEXT PRIMARY KEY, token TEXT NOT NULL, state TEXT NOT NULL CHECK(state IN ('enrolling','ready')));
CREATE TABLE storage_inventory_session(singleton INTEGER PRIMARY KEY CHECK(singleton=1), token TEXT NOT NULL);
INSERT INTO storage_inventory_session VALUES(1,'');
CREATE TABLE storage_inventory (
 storage_id TEXT PRIMARY KEY, mapping_revision INTEGER NOT NULL,
 session TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'pending', reason TEXT NOT NULL DEFAULT '',
 cursor INTEGER NOT NULL DEFAULT -1, observed_at_ms INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE file_inventory (
 hash TEXT NOT NULL REFERENCES torrents(hash) ON DELETE CASCADE,
 record_id TEXT NOT NULL, file_index INTEGER NOT NULL, mapping_revision INTEGER NOT NULL,
 metadata_path TEXT NOT NULL, size INTEGER NOT NULL,
 expected INTEGER NOT NULL DEFAULT 0, presence TEXT NOT NULL DEFAULT 'unknown',
 reason TEXT NOT NULL DEFAULT '', observed_at_ms INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(hash,file_index)
);
CREATE TRIGGER inventory_mapping AFTER UPDATE OF revision ON torrent_storage WHEN NEW.revision!=OLD.revision BEGIN
 UPDATE storage_inventory SET mapping_revision=NEW.revision,status='pending',cursor=-1,observed_at_ms=0 WHERE storage_id=NEW.storage_id;
 UPDATE file_inventory SET mapping_revision=NEW.revision,presence='unknown',observed_at_ms=0 WHERE hash=NEW.hash;
END;
CREATE TRIGGER inventory_version AFTER UPDATE OF status,reason ON storage_inventory WHEN OLD.status!=NEW.status OR OLD.reason!=NEW.reason BEGIN
 UPDATE read_model_version SET version=version+1;
 UPDATE torrents SET read_version=(SELECT version FROM read_model_version) WHERE hash IN (SELECT hash FROM torrent_storage WHERE storage_id=NEW.storage_id);
END;
