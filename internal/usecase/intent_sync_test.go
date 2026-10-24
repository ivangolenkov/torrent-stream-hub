package usecase

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
)

func TestSyncUsesObservationWriterNotUniversalSnapshotSave(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor, _ := completedSafetyFile(t, uc, "intent-sync", true)
	if err := uc.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.UpdateMetadata(tor.Hash, TorrentMetadata{Title: "confirmed title"}); err != nil {
		t.Fatal(err)
	}
	if err := uc.SetFilePriority(tor.Hash, 0, models.PriorityHigh); err != nil {
		t.Fatal(err)
	}
	// A universal SaveTorrent updates all of these columns even when unchanged.
	db, err := repository.NewSQLiteDB(filepath.Join(uc.engine.DownloadDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB().Exec(`CREATE TRIGGER reject_runtime_intent BEFORE UPDATE OF state,error,title,poster,data,category,source_uri,is_temporary ON torrents BEGIN SELECT RAISE(ABORT,'sync wrote intent fields'); END;
        CREATE TRIGGER reject_snapshot_files BEFORE UPDATE OF legacy_downloaded ON files BEGIN SELECT RAISE(ABORT,'sync used universal file save'); END;`); err != nil {
		t.Fatal(err)
	}
	sw := NewSyncWorker(uc)
	defer sw.Close()
	persistForTest(t, uc, tor.Hash)
	sw.Refresh()
	saved, err := repo.GetTorrent(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Downloaded != 1024 || !saved.MetadataKnown || saved.Title != "confirmed title" || saved.State != models.StatePaused || saved.Files[0].Priority != models.PriorityHigh {
		t.Fatal(saved)
	}
	_, o, err := uc.engine.CaptureObservation(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveRuntimeObservation(o); err != nil {
		t.Fatal("runtime was not bound to applied intent", err)
	}
}

func TestPauseCommitFailureIsNotDurableAndKeepsGateClosed(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor := temporaryFile(t, uc, "pause-failure", false)
	before, err := repo.GetIntent(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	db, err := repository.NewSQLiteDB(filepath.Join(uc.engine.DownloadDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB().Exec(`CREATE TRIGGER reject_pause_projection BEFORE UPDATE OF state ON torrents WHEN NEW.state='Paused' BEGIN SELECT RAISE(ABORT,'injected pause failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := uc.Pause(tor.Hash); err == nil {
		t.Fatal("reported durable Pause after DB failure")
	}
	after, err := repo.GetIntent(tor.Hash)
	if err != nil || *before != *after {
		t.Fatal("partial commit", before, after, err)
	}
	if uc.engine.GetRawTorrent(tor.Hash).DataDownloadAllowed() {
		t.Fatal("DB failure reopened gate")
	}
	view, err := uc.GetTorrent(tor.Hash)
	if err != nil || !view.Control.ReconcilePending || view.Control.AppliedRevision != 0 || view.Control.ApplyFailure == nil || view.Control.ApplyFailure.Code != "intent_commit_failed" || view.Control.DesiredRun != before.Run {
		t.Fatal("failed commit looked durably applied", view, err)
	}
	if _, err := db.DB().Exec(`DROP TRIGGER reject_pause_projection`); err != nil {
		t.Fatal(err)
	}
	if err := uc.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	view, err = uc.GetTorrent(tor.Hash)
	if err != nil || view.Control.ReconcilePending || view.Control.ApplyFailure != nil {
		t.Fatal("retry retained stale failure", view, err)
	}
	after, err = repo.GetIntent(tor.Hash)
	if err != nil || after.Run != models.RunPaused || after.Version.Revision != before.Version.Revision+1 {
		t.Fatal(after, err)
	}
}

func TestSyncDoesNotPromoteUnappliedCommittedRevision(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor, _ := completedSafetyFile(t, uc, "unapplied", false)
	sw := NewSyncWorker(uc)
	defer sw.Close()
	persistForTest(t, uc, tor.Hash)
	sw.Refresh()
	i, err := repo.GetIntent(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate successful intent commit followed by failed runtime apply.
	if _, err := repo.CommitRunIntent(i.Version, models.RunPaused); err != nil {
		t.Fatal(err)
	}
	_, old, err := uc.engine.CaptureObservation(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	sw.Refresh()
	_, current := uc.engine.RuntimeBinding(tor.Hash)
	if current != old.Fence {
		t.Fatal("sync relabelled stale runtime with latest committed revision")
	}
	if err := repo.SaveRuntimeObservation(old); !errors.Is(err, repository.ErrStaleObservation) {
		t.Fatal(err)
	}
}

func TestRestoreUsesIntentRatherThanLastOperationalState(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor := temporaryFile(t, uc, "intent-restore", false)
	if err := uc.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	before, err := repo.GetIntent(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := uc.engine.Delete(tor.Hash); err != nil {
		t.Fatal(err)
	}
	// Simulate stale legacy operational data without changing durable run intent.
	db, err := repository.NewSQLiteDB(filepath.Join(uc.engine.DownloadDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB().Exec(`UPDATE torrents SET state='Downloading' WHERE hash=?`, tor.Hash); err != nil {
		t.Fatal(err)
	}
	if err := uc.RestoreTorrents(); err != nil {
		t.Fatal(err)
	}
	raw := uc.engine.GetRawTorrent(tor.Hash)
	if raw == nil || raw.NetworkingEnabled() || raw.DataDownloadAllowed() || uc.engine.GetTorrent(tor.Hash).State != models.StatePaused {
		t.Fatal("stale operational state resumed torrent")
	}
	after, err := repo.GetIntent(tor.Hash)
	if err != nil || *after != *before {
		t.Fatal("restore mutated intent", after, err)
	}
}

func TestVerificationBlockSurvivesRealReopenAndClearsOnlyAfterSuccessfulCheck(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor, _ := completedSafetyFile(t, uc, "verification-reopen", false)
	if err := uc.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(uc.engine.DownloadDir(), "hub.db")
	db, err := repository.NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	if _, err := db.DB().Exec(`CREATE TRIGGER reject_verification_clear BEFORE DELETE ON torrent_activation_blocks WHEN OLD.kind='verification' BEGIN SELECT RAISE(ABORT,'injected verifier result persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.Recheck(tor.Hash); err != nil {
		t.Fatal(err)
	}
	waitStatus := func(e *engine.Engine, want string) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			model := e.GetTorrent(tor.Hash)
			if model != nil && model.Recheck != nil && model.Recheck.Status == want {
				return
			}
			select {
			case <-deadline:
				t.Fatal("verification did not reach", want)
			default:
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitStatus(uc.engine, "failed")
	i, err := repo.GetIntent(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := repo.ActivationBlocks(i.Version)
	if err != nil || len(blocks) != 1 || blocks[0].Kind != models.BlockVerification {
		t.Fatal(blocks, err)
	}
	if ready, _, err := uc.engine.CheckFileDirectReady(tor.Hash, 0); err != nil || ready {
		t.Fatal("failed result persistence restored readiness", err)
	}
	cleanup()
	db.Close()
	db, err = repository.NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`DROP TRIGGER reject_verification_clear`); err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(&config.Config{DownloadDir: uc.engine.DownloadDir(), DBPath: path, BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	restored := NewTorrentUseCase(e, repository.NewTorrentRepo(db))
	if err := restored.RestoreTorrents(); err != nil {
		t.Fatal(err)
	}
	raw := e.GetRawTorrent(tor.Hash)
	if raw == nil || raw.NetworkingEnabled() || raw.DataDownloadAllowed() {
		t.Fatal("reopen activated blocked runtime")
	}
	if ready, _, err := e.CheckFileDirectReady(tor.Hash, 0); err != nil || ready {
		t.Fatal("reopen discarded verification block", err)
	}
	if err := restored.Resume(tor.Hash); err == nil {
		t.Fatal("Resume bypassed durable verification block")
	}
	if _, err := restored.Recheck(tor.Hash); err != nil {
		t.Fatal(err)
	}
	waitStatus(e, "succeeded")
	if model := e.GetTorrent(tor.Hash); model.State != models.StatePaused || model.Error != models.ErrNone {
		t.Fatal("restored verification error swallowed durable Pause", model)
	}
	blocks, err = repository.NewTorrentRepo(db).ActivationBlocks(i.Version)
	if err != nil || len(blocks) != 0 {
		t.Fatal(blocks, err)
	}
	if raw.NetworkingEnabled() || raw.DataDownloadAllowed() {
		t.Fatal("successful local verification enabled networking")
	}
}
