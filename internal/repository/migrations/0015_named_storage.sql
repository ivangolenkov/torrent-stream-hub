-- Aliases retain their compatible rows and share one opaque owner identity.
-- Physical layout fields change together under a fenced group transaction.
ALTER TABLE torrent_storage ADD COLUMN storage_id TEXT NOT NULL DEFAULT '';
ALTER TABLE torrent_storage ADD COLUMN revision INTEGER NOT NULL DEFAULT 1;
ALTER TABLE torrent_storage ADD COLUMN prefix TEXT NOT NULL DEFAULT '';
ALTER TABLE torrent_storage ADD COLUMN name_hint TEXT NOT NULL DEFAULT '';
UPDATE torrent_storage SET storage_id=lower(hex(randomblob(16)));
UPDATE torrent_storage SET storage_id=(SELECT x.storage_id FROM torrent_storage x WHERE x.root=torrent_storage.root ORDER BY x.hash LIMIT 1) WHERE root!='';
CREATE INDEX torrent_storage_owner ON torrent_storage(storage_id);
CREATE TRIGGER storage_owner_insert AFTER INSERT ON torrent_storage WHEN NEW.storage_id='' BEGIN
 UPDATE torrent_storage SET storage_id=COALESCE((SELECT storage_id FROM torrent_storage WHERE hash!=NEW.hash AND root=NEW.root AND root!='' AND storage_id!='' LIMIT 1),lower(hex(randomblob(16)))) WHERE hash=NEW.hash;
END;
CREATE TRIGGER storage_mapping_revision AFTER UPDATE OF root,version,prefix ON torrent_storage
 WHEN OLD.root IS NOT NEW.root OR OLD.version IS NOT NEW.version OR OLD.prefix IS NOT NEW.prefix BEGIN
 SELECT CASE WHEN OLD.revision>=9223372036854775807 THEN RAISE(ABORT,'mapping revision exhausted') END;
 UPDATE torrent_storage SET revision=OLD.revision+1 WHERE hash=NEW.hash;
END;
CREATE TRIGGER storage_mapping_identity_version AFTER UPDATE OF storage_id,revision,prefix,name_hint ON torrent_storage
 WHEN OLD.storage_id IS NOT NEW.storage_id OR OLD.revision IS NOT NEW.revision OR OLD.prefix IS NOT NEW.prefix OR OLD.name_hint IS NOT NEW.name_hint BEGIN
 UPDATE storage_catalog_version SET version=version+1;
 UPDATE read_model_version SET version=version+1;
 UPDATE torrents SET read_version=(SELECT version FROM read_model_version) WHERE hash=NEW.hash;
END;
