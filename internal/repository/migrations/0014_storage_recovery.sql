-- Ownership ordering is independent of telemetry and intent application.
CREATE TABLE storage_catalog_version(singleton INTEGER PRIMARY KEY CHECK(singleton=1), version INTEGER NOT NULL);
INSERT INTO storage_catalog_version VALUES(1,1);
CREATE TRIGGER ownership_torrent_insert AFTER INSERT ON torrents BEGIN UPDATE storage_catalog_version SET version=version+1; END;
CREATE TRIGGER ownership_torrent_delete AFTER DELETE ON torrents BEGIN UPDATE storage_catalog_version SET version=version+1; END;
CREATE TRIGGER ownership_cleanup AFTER UPDATE OF cleanup_paths,cleanup_ready ON torrents WHEN OLD.cleanup_paths IS NOT NEW.cleanup_paths OR OLD.cleanup_ready IS NOT NEW.cleanup_ready BEGIN UPDATE storage_catalog_version SET version=version+1; END;
CREATE TRIGGER ownership_mapping_insert AFTER INSERT ON torrent_storage BEGIN UPDATE storage_catalog_version SET version=version+1; END;
CREATE TRIGGER ownership_mapping_delete AFTER DELETE ON torrent_storage BEGIN UPDATE storage_catalog_version SET version=version+1; END;
CREATE TRIGGER ownership_mapping_update AFTER UPDATE OF root,version,state,manifest ON torrent_storage WHEN OLD.root IS NOT NEW.root OR OLD.version IS NOT NEW.version OR OLD.state IS NOT NEW.state OR OLD.manifest IS NOT NEW.manifest BEGIN UPDATE storage_catalog_version SET version=version+1; END;
CREATE TRIGGER ownership_files_insert AFTER INSERT ON files BEGIN UPDATE storage_catalog_version SET version=version+1; END;
CREATE TRIGGER ownership_files_delete AFTER DELETE ON files BEGIN UPDATE storage_catalog_version SET version=version+1; END;
CREATE TRIGGER ownership_files_update AFTER UPDATE OF path,size ON files WHEN OLD.path IS NOT NEW.path OR OLD.size IS NOT NEW.size BEGIN UPDATE storage_catalog_version SET version=version+1; END;

-- One durable operation per record. Deletion and legacy recovery share fencing.
CREATE TABLE storage_recovery (
 hash TEXT PRIMARY KEY REFERENCES torrents(hash) ON DELETE CASCADE,
 record_id TEXT NOT NULL,
 version INTEGER NOT NULL DEFAULT 1,
 reason TEXT NOT NULL DEFAULT '',
 mode TEXT NOT NULL DEFAULT '',
 stage TEXT NOT NULL DEFAULT '',
 removed_data TEXT NOT NULL DEFAULT 'unknown',
 error TEXT NOT NULL DEFAULT '',
 failure_code TEXT NOT NULL DEFAULT '',
 journal TEXT NOT NULL DEFAULT ''
);
INSERT INTO storage_recovery(hash,record_id,reason,stage,mode)
 SELECT t.hash,i.record_id,'deletion_pending',CASE WHEN t.cleanup_ready THEN 'manifest_ready' ELSE 'intent' END,t.deletion_mode
 FROM torrents t JOIN torrent_intents i ON i.hash=t.hash WHERE t.deletion_mode!='';
CREATE TRIGGER version_storage_recovery_insert AFTER INSERT ON storage_recovery BEGIN UPDATE read_model_version SET version=version+1; UPDATE torrents SET read_version=(SELECT version FROM read_model_version) WHERE hash=NEW.hash; END;
CREATE TRIGGER version_storage_recovery_update AFTER UPDATE ON storage_recovery BEGIN UPDATE read_model_version SET version=version+1; UPDATE torrents SET read_version=(SELECT version FROM read_model_version) WHERE hash=NEW.hash; END;
CREATE TRIGGER version_storage_recovery_delete AFTER DELETE ON storage_recovery BEGIN UPDATE read_model_version SET version=version+1; UPDATE torrents SET read_version=(SELECT version FROM read_model_version) WHERE hash=OLD.hash; END;
CREATE TRIGGER recovery_deletion_intent AFTER UPDATE OF deletion_mode ON torrents WHEN OLD.deletion_mode='' AND NEW.deletion_mode!='' BEGIN
 INSERT INTO storage_recovery(hash,record_id,version,reason,stage,removed_data,mode)
 SELECT NEW.hash,record_id,1,'deletion_pending','intent','none',NEW.deletion_mode FROM torrent_intents WHERE hash=NEW.hash
 ON CONFLICT(hash) DO UPDATE SET version=storage_recovery.version+1,reason='deletion_pending',stage='intent',removed_data='none',error='',failure_code='',mode=excluded.mode;
END;
