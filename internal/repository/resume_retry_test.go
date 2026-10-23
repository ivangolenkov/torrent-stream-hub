package repository

import (
	"errors"
	"testing"
	"torrent-stream-hub/internal/models"
)

func TestResumeRetryClearsOnlyAuthorizedBlocksAtomically(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	r := NewTorrentRepo(db)
	i := intentFixture(t, r, "retry")
	f, err := r.AttachRuntime(i.Version)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []models.ActivationBlockKind{models.BlockLegacyError, models.BlockMissingFiles, models.BlockVerification} {
		if err := r.PutActivationBlock(f, kind, "test recovery required"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.CommitResumeRetry(i.Version); !errors.Is(err, ErrStateConflict) {
		t.Fatal("retry bypassed verification", err)
	}
	blocks, err := r.ActivationBlocks(i.Version)
	if err != nil || len(blocks) != 3 {
		t.Fatal("failed retry removed blocks", blocks, err)
	}
	if err := r.ClearActivationBlock(f, models.BlockVerification); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CommitRunIntent(i.Version, models.RunEnabled); !errors.Is(err, ErrStateConflict) {
		t.Fatal("ordinary intent command became repair authorization", err)
	}
	if _, err := db.DB().Exec(`CREATE TRIGGER reject_retry_commit BEFORE UPDATE OF state ON torrents BEGIN SELECT RAISE(ABORT,'injected retry commit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CommitResumeRetry(i.Version); err == nil {
		t.Fatal("ignored commit failure")
	}
	blocks, err = r.ActivationBlocks(i.Version)
	if err != nil || len(blocks) != 2 {
		t.Fatal("commit failure removed safety blocks", blocks, err)
	}
	after, err := r.GetIntent(i.Version.Hash)
	if err != nil || *after != *i {
		t.Fatal(after, err)
	}
	if _, err := db.DB().Exec(`DROP TRIGGER reject_retry_commit`); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveDownloadBlock(i.Version.Hash, "disk latch"); err != nil {
		t.Fatal(err)
	}
	v, err := r.CommitResumeRetry(i.Version)
	if err != nil || v.Revision != i.Version.Revision+1 {
		t.Fatal(v, err)
	}
	blocks, err = r.ActivationBlocks(v)
	if err != nil || len(blocks) != 0 {
		t.Fatal(blocks, err)
	}
	if block, err := r.DownloadBlock(v.Hash); err != nil || block != "disk latch" {
		t.Fatal("retry cleared disk latch without engine probe", block, err)
	}
	if _, err := r.CommitResumeRetry(i.Version); !errors.Is(err, ErrStateConflict) {
		t.Fatal("accepted stale retry", err)
	}
}
