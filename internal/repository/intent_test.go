package repository

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"torrent-stream-hub/internal/models"
)

func intentFixture(t *testing.T, r *TorrentRepo, hash string) *models.TorrentIntent {
	t.Helper()
	if err := r.SaveTorrentWithPolicy(&models.Torrent{Hash: hash, Name: "catalog", Title: "user title", SourceURI: "magnet:original", State: models.StatePaused, Size: 8, Files: []*models.File{{Index: 0, Path: "video", Size: 8, Priority: models.PriorityHigh}}}, true); err != nil {
		t.Fatal(err)
	}
	i, err := r.GetIntent(hash)
	if err != nil || i == nil {
		t.Fatal(i, err)
	}
	return i
}

func verifiedObservation(f models.RuntimeFence, seq, bytes, at int64) models.RuntimeObservation {
	return models.RuntimeObservation{Fence: f, Sequence: seq, AppliedRevision: f.Revision, State: models.StateDownloading, ProgressSource: "verified", ObservedAt: at, VerifiedBytes: bytes, Files: []models.FileObservation{{Index: 0, VerifiedBytes: bytes}}}
}

func TestRuntimeObservationIsNarrowAndSequenceOrdered(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	r := NewTorrentRepo(db)
	i := intentFixture(t, r, "observed")
	f, err := r.AttachRuntime(i.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SaveRuntimeObservation(verifiedObservation(f, 1, 8, 100)); err != nil {
		t.Fatal(err)
	}
	// A clock regression cannot restore old bytes or prevent an actual decrease.
	if err := r.SaveRuntimeObservation(verifiedObservation(f, 2, 0, 50)); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveRuntimeObservation(verifiedObservation(f, 1, 8, 200)); !errors.Is(err, ErrStaleObservation) {
		t.Fatal(err)
	}
	saved, err := r.GetTorrent(i.Version.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Downloaded != 0 || saved.ProgressObservedAt != 50 || saved.State != models.StatePaused || saved.Title != "user title" || saved.SourceURI != "magnet:original" || !saved.IsTemporary || saved.Files[0].Priority != models.PriorityHigh {
		t.Fatal("observation overwrote authority", saved)
	}
	intent, err := r.GetIntent(i.Version.Hash)
	if err != nil || *intent != *i {
		t.Fatal(intent, err)
	}
	var state string
	if err := db.DB().QueryRow(`SELECT observed_state FROM torrent_intents WHERE hash=?`, i.Version.Hash).Scan(&state); err != nil || state != "Downloading" {
		t.Fatal(state, err)
	}
}

func TestObservationFencesRevisionAttachmentAndReadd(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	r := NewTorrentRepo(db)
	i := intentFixture(t, r, "readd")
	f, err := r.AttachRuntime(i.Version)
	if err != nil {
		t.Fatal(err)
	}
	other, err := r.AttachRuntime(i.Version)
	if err != nil {
		t.Fatal(err)
	}
	if f.Attachment == other.Attachment {
		t.Fatal("reused attachment")
	}
	if err := r.SaveRuntimeObservation(verifiedObservation(f, 99, 8, 99)); !errors.Is(err, ErrStaleObservation) {
		t.Fatal(err)
	}
	v, err := r.CommitRunIntent(i.Version, models.RunEnabled)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.CommitRunIntent(i.Version, models.RunPaused); !errors.Is(err, ErrStateConflict) {
		t.Fatal(err)
	}
	if err := r.SaveRuntimeObservation(verifiedObservation(other, 1, 8, 100)); !errors.Is(err, ErrStaleObservation) {
		t.Fatal(err)
	}
	newFence, err := r.AttachRuntime(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteTorrent(v.Hash); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveRuntimeObservation(verifiedObservation(newFence, 1, 8, 100)); !errors.Is(err, ErrStaleObservation) {
		t.Fatal(err)
	}
	if i, err := r.GetTorrent(v.Hash); err != nil || i != nil {
		t.Fatal("resurrected missing record", i, err)
	}
	newIntent := intentFixture(t, r, v.Hash)
	if newIntent.Version.RecordID == v.RecordID {
		t.Fatal("readd reused identity")
	}
	if err := r.SaveRuntimeObservation(verifiedObservation(newFence, 1000, 8, 200)); !errors.Is(err, ErrStaleObservation) {
		t.Fatal(err)
	}
}

func TestObservationRollbackAndWorkflowConflicts(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	r := NewTorrentRepo(db)
	i := intentFixture(t, r, "rollback")
	f, err := r.AttachRuntime(i.Version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`CREATE TRIGGER reject_observed_file BEFORE UPDATE OF downloaded ON files BEGIN SELECT RAISE(ABORT,'injected file failure'); END`); err != nil {
		t.Fatal(err)
	}
	o := verifiedObservation(f, 1, 8, 10)
	if err := r.SaveRuntimeObservation(o); err == nil {
		t.Fatal("ignored file failure")
	}
	var seq, bytes int64
	if err := db.DB().QueryRow(`SELECT i.observation_sequence,t.downloaded FROM torrent_intents i JOIN torrents t ON t.hash=i.hash WHERE t.hash=?`, i.Version.Hash).Scan(&seq, &bytes); err != nil || seq != 0 || bytes != 0 {
		t.Fatal(seq, bytes, err)
	}
	if _, err := db.DB().Exec(`DROP TRIGGER reject_observed_file`); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveRuntimeObservation(o); err != nil {
		t.Fatal(err)
	}
	o.Sequence = 2
	o.Files = []models.FileObservation{{Index: 99, VerifiedBytes: 8}}
	if err := r.SaveRuntimeObservation(o); !errors.Is(err, ErrFileNotFound) {
		t.Fatal(err)
	}
	if err := r.BeginForget(i.Version.Hash); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CommitRunIntent(i.Version, models.RunEnabled); !errors.Is(err, ErrStateConflict) {
		t.Fatal(err)
	}
	if _, err := r.AttachRuntime(i.Version); !errors.Is(err, ErrStateConflict) {
		t.Fatal(err)
	}
	if err := r.SaveRuntimeObservation(verifiedObservation(f, 2, 8, 20)); !errors.Is(err, ErrStaleObservation) {
		t.Fatal(err)
	}
}

func TestIntentUpgradeBackfillAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	r := NewTorrentRepo(db)
	states := []models.TorrentState{models.StatePaused, models.StateQueued, models.StateDownloading, models.StateSeeding, models.StateStreaming, models.StateError, models.StateMissingFiles, models.StateDiskFull, "unexpected"}
	for _, state := range states {
		if err := r.SaveTorrent(&models.Torrent{Hash: string(state), State: state, Name: string(state), Size: 8, Downloaded: 4, Files: []*models.File{{Index: 0, Size: 8, Priority: models.PriorityNone}}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB().Exec(`DROP TRIGGER torrents_initialize_intent; DROP TABLE torrent_activation_blocks; DROP TABLE torrent_intents; DELETE FROM migrations WHERE name='0009_intent_and_runtime_fences.sql'; DELETE FROM torrent_download_blocks`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	r = NewTorrentRepo(db)
	identities := make(map[string]bool)
	for _, state := range states {
		i, err := r.GetIntent(string(state))
		if err != nil || i == nil {
			t.Fatal(i, err)
		}
		want := models.RunPaused
		switch state {
		case models.StateQueued, models.StateDownloading, models.StateSeeding, models.StateStreaming, models.StateDiskFull:
			want = models.RunEnabled
		}
		if i.Run != want || i.Version.Revision != 1 || len(i.Version.RecordID) != 32 || identities[i.Version.RecordID] {
			t.Fatal(i)
		}
		identities[i.Version.RecordID] = true
		saved, err := r.GetTorrent(string(state))
		if err != nil || saved.Downloaded != 4 || saved.Files[0].Priority != models.PriorityNone {
			t.Fatal(saved, err)
		}
	}
	block, err := r.DownloadBlock("DiskFull")
	if err != nil || block == "" {
		t.Fatal("missing legacy latch", block, err)
	}
	var blocks int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM torrent_activation_blocks`).Scan(&blocks); err != nil || blocks != 2 {
		t.Fatal(blocks, err)
	}
	i, err := r.GetIntent("Paused")
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.CommitRunIntent(i.Version, models.RunEnabled)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	after, err := NewTorrentRepo(db).GetIntent("Paused")
	if err != nil || after.Version != v || after.Run != models.RunEnabled {
		t.Fatal(after, err)
	}
}

func TestActivationBlocksAreIndependentAndAttachmentFenced(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	r := NewTorrentRepo(db)
	i := intentFixture(t, r, "blocks")
	f, err := r.AttachRuntime(i.Version)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []models.ActivationBlockKind{models.BlockVerification, models.BlockMissingFiles} {
		if err := r.PutActivationBlock(f, kind, "requires explicit recovery"); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.SaveDownloadBlock(f.Hash, "Disk full"); err != nil {
		t.Fatal(err)
	}
	current, err := r.AttachRuntime(i.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ClearActivationBlock(f, models.BlockVerification); !errors.Is(err, ErrStateConflict) {
		t.Fatal(err)
	}
	if err := r.PutActivationBlock(f, models.BlockVerification, "stale error"); !errors.Is(err, ErrStateConflict) {
		t.Fatal(err)
	}
	if err := r.ClearActivationBlock(current, models.BlockVerification); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM torrent_activation_blocks WHERE record_id=? AND kind='missing_files'`, i.Version.RecordID).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if block, err := r.DownloadBlock(f.Hash); err != nil || block != "Disk full" {
		t.Fatal(block, err)
	}
	if after, err := r.GetIntent(f.Hash); err != nil || *after != *i {
		t.Fatal(after, err)
	}
	if err := r.DeleteTorrent(f.Hash); err != nil {
		t.Fatal(err)
	}
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM torrent_activation_blocks`).Scan(&count); err != nil || count != 0 {
		t.Fatal("block cascade failed", count, err)
	}
}

func TestIntentMigrationFailureRollsBackSchemaAndBackfill(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	r := NewTorrentRepo(db)
	intentFixture(t, r, "upgrade-failure")
	if _, err := db.DB().Exec(`DROP TRIGGER torrents_initialize_intent; DROP TABLE torrent_activation_blocks; DROP TABLE torrent_intents;
        DELETE FROM migrations WHERE name='0009_intent_and_runtime_fences.sql';
        CREATE TABLE torrent_activation_blocks(sentinel TEXT); INSERT INTO torrent_activation_blocks VALUES('keep me');`); err != nil {
		t.Fatal(err)
	}
	if err := db.runMigrations(); err == nil {
		t.Fatal("expected mid-migration schema failure")
	}
	var count int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('torrent_intents','torrents_initialize_intent')`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial schema survived rollback", count, err)
	}
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM migrations WHERE name='0009_intent_and_runtime_fences.sql'`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	var sentinel string
	if err := db.DB().QueryRow(`SELECT sentinel FROM torrent_activation_blocks`).Scan(&sentinel); err != nil || sentinel != "keep me" {
		t.Fatal(sentinel, err)
	}
	if _, err := db.DB().Exec(`DROP TABLE torrent_activation_blocks`); err != nil {
		t.Fatal(err)
	}
	if err := db.runMigrations(); err != nil {
		t.Fatal("retry failed", err)
	}
	if i, err := r.GetIntent("upgrade-failure"); err != nil || i == nil || i.Run != models.RunPaused {
		t.Fatal(i, err)
	}
}

func TestConcurrentIntentCommandsHaveSingleRevisionWinner(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	r := NewTorrentRepo(db)
	i := intentFixture(t, r, "revision-race")
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, run := range []models.RunIntent{models.RunPaused, models.RunEnabled} {
		wg.Add(1)
		go func(run models.RunIntent) {
			defer wg.Done()
			<-start
			_, err := r.CommitRunIntent(i.Version, run)
			results <- err
		}(run)
	}
	close(start)
	wg.Wait()
	close(results)
	winners, conflicts := 0, 0
	for err := range results {
		if err == nil {
			winners++
		} else if errors.Is(err, ErrStateConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatal(winners, conflicts)
	}
	after, err := r.GetIntent(i.Version.Hash)
	if err != nil || after.Version.Revision != 2 || after.Version.RecordID != i.Version.RecordID {
		t.Fatal(after, err)
	}
}
