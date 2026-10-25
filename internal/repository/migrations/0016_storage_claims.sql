-- Durable namespace reservations share the recovery record's identity/version.
-- Retained locations deliberately outlive deletion of a torrent record.
-- Polling REST/SSE reads this small projection, never the large file journal.
ALTER TABLE storage_recovery ADD COLUMN location_source TEXT NOT NULL DEFAULT '';
ALTER TABLE storage_recovery ADD COLUMN location_target TEXT NOT NULL DEFAULT '';
ALTER TABLE storage_recovery ADD COLUMN location_method TEXT NOT NULL DEFAULT '';
ALTER TABLE storage_recovery ADD COLUMN location_extra_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE storage_recovery ADD COLUMN location_alternatives INTEGER NOT NULL DEFAULT 0;
ALTER TABLE storage_recovery ADD COLUMN location_alternative_bytes INTEGER NOT NULL DEFAULT 0;
CREATE TABLE storage_claims (
 storage_id TEXT NOT NULL,
 record_id TEXT NOT NULL,
 operation_version INTEGER NOT NULL,
 path TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('source','target','staging','retained')),
 PRIMARY KEY(storage_id,record_id,operation_version,path)
);
CREATE TRIGGER ownership_claim_insert AFTER INSERT ON storage_claims BEGIN UPDATE storage_catalog_version SET version=version+1; END;
CREATE TRIGGER ownership_claim_delete AFTER DELETE ON storage_claims BEGIN UPDATE storage_catalog_version SET version=version+1; END;
CREATE TRIGGER ownership_claim_update AFTER UPDATE ON storage_claims BEGIN UPDATE storage_catalog_version SET version=version+1; END;
