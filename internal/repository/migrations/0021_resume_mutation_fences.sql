-- Recovery journals are durable admission records. Publication may never survive
-- a workflow that has begun changing payload, even if that workflow later clears
-- its journal without changing mapping geometry.
CREATE TRIGGER resume_journal_insert BEFORE INSERT ON storage_recovery
WHEN NEW.stage NOT IN ('planned','analyzed','completed','cancelled')
BEGIN
    SELECT CASE WHEN EXISTS(SELECT 1 FROM resume_owners WHERE record_id=NEW.record_id AND mutation_epoch=9223372036854775807) THEN RAISE(ABORT,'resume epoch exhausted') END;
    UPDATE resume_owners SET invalidated=1,mutation_epoch=mutation_epoch+1 WHERE record_id=NEW.record_id AND storage_id IN (SELECT storage_id FROM torrent_storage WHERE hash=NEW.hash);
END;
CREATE TRIGGER resume_journal_update BEFORE UPDATE ON storage_recovery
WHEN NEW.stage NOT IN ('planned','analyzed','completed','cancelled')
BEGIN
    SELECT CASE WHEN EXISTS(SELECT 1 FROM resume_owners WHERE record_id=NEW.record_id AND mutation_epoch=9223372036854775807) THEN RAISE(ABORT,'resume epoch exhausted') END;
    UPDATE resume_owners SET invalidated=1,mutation_epoch=mutation_epoch+1 WHERE record_id=NEW.record_id AND storage_id IN (SELECT storage_id FROM torrent_storage WHERE hash=NEW.hash);
END;
CREATE TRIGGER resume_mapping_workflow BEFORE UPDATE OF state,manifest ON torrent_storage
WHEN (NEW.state!='ready' OR NEW.manifest!='') AND (OLD.state IS NOT NEW.state OR OLD.manifest IS NOT NEW.manifest)
BEGIN
    SELECT CASE WHEN EXISTS(SELECT 1 FROM resume_owners WHERE storage_id=OLD.storage_id AND mutation_epoch=9223372036854775807) THEN RAISE(ABORT,'resume epoch exhausted') END;
    UPDATE resume_owners SET invalidated=1,mutation_epoch=mutation_epoch+1 WHERE storage_id=OLD.storage_id;
END;
