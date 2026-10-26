-- A retained head fences callbacks even after record deletion/re-add. Hints in
-- piece_completion are intentionally not imported. Payload flush is the caller's
-- responsibility before CommitResume publishes a new generation.
CREATE TABLE resume_owners (
    storage_id TEXT NOT NULL PRIMARY KEY CHECK(storage_id!='' AND length(CAST(storage_id AS BLOB))<=256),
    record_id TEXT NOT NULL CHECK(record_id!='' AND length(CAST(record_id AS BLOB))<=256),
    mapping_revision INTEGER NOT NULL CHECK(typeof(mapping_revision)='integer' AND mapping_revision>0),
    mutation_epoch INTEGER NOT NULL CHECK(typeof(mutation_epoch)='integer' AND mutation_epoch>0),
    generation INTEGER NOT NULL DEFAULT 0 CHECK(typeof(generation)='integer' AND generation>=0),
    invalidated INTEGER NOT NULL DEFAULT 1 CHECK(invalidated IN (0,1))
);
CREATE TABLE resume_checkpoints (
    storage_id TEXT NOT NULL PRIMARY KEY REFERENCES resume_owners(storage_id) ON DELETE CASCADE,
    payload BLOB NOT NULL CHECK(typeof(payload)='blob' AND length(payload)<=4194304),
    revoked BLOB NOT NULL CHECK(typeof(revoked)='blob' AND length(revoked)<=524288),
    integrity BLOB NOT NULL CHECK(typeof(integrity)='blob' AND length(integrity)=32)
);

-- Defensive DB fencing for mutations that change authoritative identity. Normal
-- payload writes still require their own durable revocation hook (phase 43/4).
CREATE TRIGGER resume_mapping_change BEFORE UPDATE OF storage_id,revision,root,version,prefix ON torrent_storage
WHEN OLD.storage_id IS NOT NEW.storage_id OR OLD.revision IS NOT NEW.revision OR OLD.root IS NOT NEW.root OR OLD.version IS NOT NEW.version OR OLD.prefix IS NOT NEW.prefix
BEGIN
    SELECT CASE WHEN EXISTS(SELECT 1 FROM resume_owners WHERE storage_id=OLD.storage_id AND mutation_epoch=9223372036854775807) THEN RAISE(ABORT,'resume epoch exhausted') END;
    UPDATE resume_owners SET invalidated=1,mutation_epoch=mutation_epoch+1 WHERE storage_id=OLD.storage_id;
END;
CREATE TRIGGER resume_mapping_delete BEFORE DELETE ON torrent_storage BEGIN
    SELECT CASE WHEN EXISTS(SELECT 1 FROM resume_owners WHERE storage_id=OLD.storage_id AND mutation_epoch=9223372036854775807) THEN RAISE(ABORT,'resume epoch exhausted') END;
    UPDATE resume_owners SET invalidated=1,mutation_epoch=mutation_epoch+1 WHERE storage_id=OLD.storage_id;
END;
CREATE TRIGGER resume_record_delete BEFORE DELETE ON torrent_intents BEGIN
    SELECT CASE WHEN EXISTS(SELECT 1 FROM resume_owners WHERE record_id=OLD.record_id AND mutation_epoch=9223372036854775807) THEN RAISE(ABORT,'resume epoch exhausted') END;
    UPDATE resume_owners SET invalidated=1,mutation_epoch=mutation_epoch+1 WHERE record_id=OLD.record_id;
END;
CREATE TRIGGER resume_record_change BEFORE UPDATE OF record_id ON torrent_intents WHEN OLD.record_id IS NOT NEW.record_id BEGIN
    SELECT CASE WHEN EXISTS(SELECT 1 FROM resume_owners WHERE record_id=OLD.record_id AND mutation_epoch=9223372036854775807) THEN RAISE(ABORT,'resume epoch exhausted') END;
    UPDATE resume_owners SET invalidated=1,mutation_epoch=mutation_epoch+1 WHERE record_id=OLD.record_id;
END;
CREATE TRIGGER resume_deletion_intent BEFORE UPDATE OF deletion_mode ON torrents WHEN NEW.deletion_mode!='' AND OLD.deletion_mode IS NOT NEW.deletion_mode BEGIN
    SELECT CASE WHEN EXISTS(SELECT 1 FROM resume_owners r JOIN torrent_intents i ON i.record_id=r.record_id WHERE i.hash=OLD.hash AND r.mutation_epoch=9223372036854775807) THEN RAISE(ABORT,'resume epoch exhausted') END;
    UPDATE resume_owners SET invalidated=1,mutation_epoch=mutation_epoch+1 WHERE record_id IN (SELECT record_id FROM torrent_intents WHERE hash=OLD.hash);
END;
