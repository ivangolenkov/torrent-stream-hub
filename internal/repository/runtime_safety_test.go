package repository

import (
	"errors"
	"path/filepath"
	"testing"

	"torrent-stream-hub/internal/models"
)

func TestRuntimeSafetyIsAttachmentScopedAcrossCommandCommit(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	r := NewTorrentRepo(db)
	i := intentFixture(t, r, "runtime-safety")
	f, err := r.AttachRuntime(i.Version)
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.CommitRunIntent(i.Version, models.RunPaused)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []models.ActivationBlockKind{models.BlockMetadataPersistence, models.BlockInvalidSelection, models.BlockMissingFiles} {
		if err := r.PutRuntimeSafetyBlock(f, kind, "bounded fault"); err != nil {
			t.Fatal("fault lost between commit/apply", err)
		}
	}
	blocks, err := r.ActivationBlocks(v)
	if err != nil || len(blocks) != 3 {
		t.Fatal(blocks, err)
	}
	if err := r.PutRuntimeSafetyBlock(f, models.BlockVerification, "not this writer"); err == nil {
		t.Fatal("runtime safety writer gained verification authority")
	}
	newFence, err := r.AttachRuntime(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.PutRuntimeSafetyBlock(f, models.BlockMissingFiles, "late"); !errors.Is(err, ErrStateConflict) {
		t.Fatal("old attachment wrote", err)
	}
	if _, err := r.CommitRunIntent(v, models.RunEnabled); !errors.Is(err, ErrStateConflict) {
		t.Fatal("enabled without explicit retry", err)
	}
	if _, err := db.DB().Exec(`CREATE TRIGGER reject_safety_retry BEFORE UPDATE OF state ON torrents BEGIN SELECT RAISE(ABORT,'retry failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CommitResumeRetry(v); err == nil {
		t.Fatal("ignored SQLite failure")
	}
	blocks, err = r.ActivationBlocks(v)
	if err != nil || len(blocks) != 3 {
		t.Fatal("failed retry cleared safety", blocks, err)
	}
	if _, err := db.DB().Exec(`DROP TRIGGER reject_safety_retry`); err != nil {
		t.Fatal(err)
	}
	v, err = r.CommitResumeRetry(v)
	if err != nil {
		t.Fatal(err)
	}
	blocks, err = r.ActivationBlocks(v)
	if err != nil || len(blocks) != 0 {
		t.Fatal(blocks, err)
	}
	if err := r.DeleteTorrent(v.Hash); err != nil {
		t.Fatal(err)
	}
	intentFixture(t, r, v.Hash)
	if err := r.PutRuntimeSafetyBlock(newFence, models.BlockMissingFiles, "readd stale"); !errors.Is(err, ErrStateConflict) {
		t.Fatal("old record wrote", err)
	}
}

func TestTypedFaultMigrationPreservesV9KeysAndRollsBackOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewTorrentRepo(db)
	i := intentFixture(t, r, "upgrade-safety")
	f, err := r.AttachRuntime(i.Version)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []models.ActivationBlockKind{models.BlockVerification, models.BlockMissingFiles, models.BlockLegacyError} {
		if err := r.PutActivationBlock(f, kind, string(kind)); err != nil {
			t.Fatal(err)
		}
	}
	// Reconstruct the released v9 table, not an empty stand-in. Preserve the
	// parent identity, attachment, intent and all existing keys during upgrade.
	if _, err := db.DB().Exec(`ALTER TABLE torrent_activation_blocks RENAME TO saved_blocks;
	CREATE TABLE torrent_activation_blocks(record_id TEXT NOT NULL REFERENCES torrent_intents(record_id) ON DELETE CASCADE,kind TEXT NOT NULL CHECK(kind IN ('verification','missing_files','legacy_error')),reason TEXT NOT NULL,PRIMARY KEY(record_id,kind));
	INSERT INTO torrent_activation_blocks SELECT * FROM saved_blocks; DROP TABLE saved_blocks;
	DELETE FROM migrations WHERE name='0010_typed_activation_faults.sql';
	CREATE TABLE torrent_activation_blocks_v10(injected TEXT);`); err != nil {
		t.Fatal(err)
	}
	if err := db.runMigrations(); err == nil {
		t.Fatal("ignored injected migration failure")
	}
	blocks, err := r.ActivationBlocks(i.Version)
	if err != nil || len(blocks) != 3 {
		t.Fatal("failed upgrade lost old keys", blocks, err)
	}
	if _, err := db.DB().Exec(`DROP TABLE torrent_activation_blocks_v10`); err != nil {
		t.Fatal(err)
	}
	if err := db.runMigrations(); err != nil {
		t.Fatal(err)
	}
	if err := r.PutRuntimeSafetyBlock(f, models.BlockMetadataPersistence, "fault"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r = NewTorrentRepo(db)
	blocks, err = r.ActivationBlocks(i.Version)
	if err != nil || len(blocks) != 4 {
		t.Fatal("upgrade/reopen lost keys", blocks, err)
	}
	if got, err := r.GetIntent(i.Version.Hash); err != nil || *got != *i {
		t.Fatal("upgrade changed intent", got, err)
	}
	if err := r.DeleteTorrent(i.Version.Hash); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM torrent_activation_blocks`).Scan(&count); err != nil || count != 0 {
		t.Fatal("upgraded foreign key cascade lost", count, err)
	}
}
