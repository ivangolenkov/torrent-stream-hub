package usecase

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
)

func TestFullRunReconcileAppliesEarlierCommittedSelection(t *testing.T) {
	for _, command := range []string{"pause", "resume"} {
		t.Run(command, func(t *testing.T) {
			uc, repo, cleanup := setupTorrentUseCase(t)
			defer cleanup()
			tor := temporaryFile(t, uc, "full-reconcile", false)
			// Commit a selection whose runtime apply did not happen. Testing the
			// engine's base priorities avoids hiding the bug behind the read mapper.
			if err := repo.UpdateFilePriority(tor.Hash, 0, models.PriorityNone); err != nil {
				t.Fatal(err)
			}
			if got := uc.engine.GetTorrent(tor.Hash).Files[0].Priority; got == models.PriorityNone {
				t.Fatal("fixture already applied selection")
			}
			var err error
			if command == "pause" {
				err = uc.Pause(tor.Hash)
			} else {
				err = uc.Resume(tor.Hash)
			}
			if err != nil {
				t.Fatal(err)
			}
			i, err := repo.GetIntent(tor.Hash)
			if err != nil {
				t.Fatal(err)
			}
			live := uc.engine.GetTorrent(tor.Hash)
			if live.Files[0].Priority != models.PriorityNone || live.Control.AppliedRevision != i.Version.Revision {
				t.Fatal("acknowledged without full selection apply", live)
			}
			if uc.engine.GetRawTorrent(tor.Hash).DataDownloadAllowed() {
				t.Fatal("Skip All admitted background payload")
			}
		})
	}
}

func TestIntentApplyFailureIsVisibleAndOnlyFullRetryAcknowledgesIt(t *testing.T) {
	commands := []struct {
		name string
		run  func(*TorrentUseCase, string) error
	}{
		{"pause", func(uc *TorrentUseCase, h string) error { return uc.Pause(h) }},
		{"resume", func(uc *TorrentUseCase, h string) error { return uc.Resume(h) }},
		{"priority", func(uc *TorrentUseCase, h string) error { return uc.SetFilePriority(h, 0, models.PriorityNone) }},
		{"bulk_priority", func(uc *TorrentUseCase, h string) error { return uc.SetTorrentFilesPriority(h, models.PriorityHigh) }},
		{"metadata", func(uc *TorrentUseCase, h string) error {
			_, err := uc.UpdateMetadata(h, TorrentMetadata{Title: "committed"})
			return err
		}},
		{"permanent", func(uc *TorrentUseCase, h string) error { return uc.MakePermanent(h) }},
	}
	for _, command := range commands {
		t.Run(command.name, func(t *testing.T) {
			uc, repo, cleanup := setupTorrentUseCase(t)
			defer cleanup()
			tor := temporaryFile(t, uc, "failed-bind", false)
			before, err := repo.GetIntent(tor.Hash)
			if err != nil {
				t.Fatal(err)
			}
			db, err := repository.NewSQLiteDB(filepath.Join(uc.engine.DownloadDir(), "hub.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.DB().Exec(`CREATE TRIGGER reject_apply_attachment BEFORE UPDATE OF runtime_attachment ON torrent_intents BEGIN SELECT RAISE(ABORT,'injected bind failure /private/secret'); END`); err != nil {
				t.Fatal(err)
			}
			if err := command.run(uc, tor.Hash); err == nil {
				t.Fatal("reported success after failed bind")
			}
			i, err := repo.GetIntent(tor.Hash)
			if err != nil || i.Version.Revision != before.Version.Revision+1 {
				t.Fatal("lost committed command", i, err)
			}
			assertFailure := func(tor *models.Torrent) {
				t.Helper()
				c := tor.Control
				if !c.ReconcilePending || c.AppliedRevision != 0 || c.ApplyFailure == nil || c.ApplyFailure.Revision != i.Version.Revision || c.ApplyFailure.Code != "runtime_bind_failed" || c.ApplyFailure.Retry != "explicit_pause_or_resume" {
					t.Fatal("lost apply failure", c)
				}
			}
			model, err := uc.GetTorrent(tor.Hash)
			if err != nil {
				t.Fatal(err)
			}
			assertFailure(model)
			raw := uc.engine.GetRawTorrent(tor.Hash)
			if raw.DataDownloadAllowed() {
				t.Fatal("failed bind left download gate open")
			}
			sw := NewSyncWorker(uc)
			defer sw.Close()
			ch := make(chan []byte, 1)
			sw.AddClient(ch)
			defer sw.RemoveClient(ch)
			sw.Refresh()
			var views []*models.Torrent
			if err := json.Unmarshal(<-ch, &views); err != nil || len(views) != 1 {
				t.Fatal(views, err)
			}
			assertFailure(views[0])
			if _, err := db.DB().Exec(`DROP TRIGGER reject_apply_attachment`); err != nil {
				t.Fatal(err)
			}
			// An unrelated successful incremental command cannot acknowledge or
			// clear the previous failure, even though its committed revision is newer.
			if _, err := uc.UpdateMetadata(tor.Hash, TorrentMetadata{Title: "later metadata"}); err != nil {
				t.Fatal(err)
			}
			model, err = uc.GetTorrent(tor.Hash)
			if err != nil {
				t.Fatal(err)
			}
			assertFailure(model)
			if raw.DataDownloadAllowed() {
				t.Fatal("metadata reopened gate")
			}
			if err := uc.Pause(tor.Hash); err != nil {
				t.Fatal("full retry failed", err)
			}
			model, err = uc.GetTorrent(tor.Hash)
			if err != nil || model.Control.ReconcilePending || model.Control.ApplyFailure != nil {
				t.Fatal(model, err)
			}
			saved, err := repo.GetTorrent(tor.Hash)
			if err != nil {
				t.Fatal(err)
			}
			if live := uc.engine.GetTorrent(tor.Hash); live.Files[0].Priority != saved.Files[0].Priority {
				t.Fatal("retry failed to reconcile saved selection")
			}
		})
	}
}

func TestCommittedResumeApplyFailureRestoresFromSQLiteOnRealReopen(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor := temporaryFile(t, uc, "reconcile-reopen", false)
	if err := uc.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(uc.engine.DownloadDir(), "hub.db")
	db, err := repository.NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	if _, err := db.DB().Exec(`CREATE TRIGGER reject_reopen_bind BEFORE UPDATE OF runtime_attachment ON torrent_intents BEGIN SELECT RAISE(ABORT,'injected bind failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := uc.Resume(tor.Hash); err == nil {
		t.Fatal("ignored failed apply")
	}
	i, err := repo.GetIntent(tor.Hash)
	if err != nil || i.Run != models.RunEnabled {
		t.Fatal(i, err)
	}
	cleanup()
	db.Close()
	db, err = repository.NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`DROP TRIGGER reject_reopen_bind`); err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(&config.Config{DownloadDir: uc.engine.DownloadDir(), DBPath: path, BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	reopenedRepo := repository.NewTorrentRepo(db)
	reopened := NewTorrentUseCase(e, reopenedRepo)
	if err := reopened.RestoreTorrents(); err != nil {
		t.Fatal(err)
	}
	after, err := reopenedRepo.GetIntent(tor.Hash)
	if err != nil || *after != *i {
		t.Fatal("reopen changed committed intent", after, err)
	}
	model, err := reopened.GetTorrent(tor.Hash)
	if err != nil || model.Control.ReconcilePending || model.Control.ApplyFailure != nil || model.Control.AppliedRevision != i.Version.Revision {
		t.Fatal(model, err)
	}
}

func TestResumeApplyFailureKeepsIndependentDiskLatch(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor := temporaryFile(t, uc, "apply-disk-latch", false)
	if err := uc.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveDownloadBlock(tor.Hash, "sticky disk latch"); err != nil {
		t.Fatal(err)
	}
	db, err := repository.NewSQLiteDB(filepath.Join(uc.engine.DownloadDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB().Exec(`CREATE TRIGGER reject_latch_clear BEFORE DELETE ON torrent_download_blocks BEGIN SELECT RAISE(ABORT,'injected disk recovery persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := uc.Resume(tor.Hash); err == nil {
		t.Fatal("reported success after failed runtime apply")
	}
	model, err := uc.GetTorrent(tor.Hash)
	if err != nil || model.Control.DesiredRun != models.RunEnabled || !model.Control.ReconcilePending || model.Control.ApplyFailure == nil || model.Control.ApplyFailure.Code != "runtime_apply_failed" {
		t.Fatal(model, err)
	}
	if uc.engine.GetRawTorrent(tor.Hash).DataDownloadAllowed() {
		t.Fatal("runtime apply failure reopened gate")
	}
	if block, err := repo.DownloadBlock(tor.Hash); err != nil || block != "sticky disk latch" {
		t.Fatal("failure cleared independent disk latch", block, err)
	}
	if _, err := db.DB().Exec(`DROP TRIGGER reject_latch_clear`); err != nil {
		t.Fatal(err)
	}
	if err := uc.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	if block, err := repo.DownloadBlock(tor.Hash); err != nil || block != "sticky disk latch" {
		t.Fatal("full Pause reconciliation cleared disk latch", block, err)
	}
	if err := uc.Resume(tor.Hash); err != nil {
		t.Fatal(err)
	}
	model, err = uc.GetTorrent(tor.Hash)
	if err != nil || model.Control.ReconcilePending || model.Control.ApplyFailure != nil {
		t.Fatal(model, err)
	}
	if block, err := repo.DownloadBlock(tor.Hash); err != nil || block != "" {
		t.Fatal("explicit retry did not finish disk recovery", block, err)
	}
}

func TestAddCommittedBindFailureStaysStagedUntilExplicitResume(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	db, err := repository.NewSQLiteDB(filepath.Join(uc.engine.DownloadDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB().Exec(`CREATE TRIGGER reject_add_bind BEFORE UPDATE OF runtime_attachment ON torrent_intents BEGIN SELECT RAISE(ABORT,'injected add bind failure'); END`); err != nil {
		t.Fatal(err)
	}
	hash := "8888888888888888888888888888888888888888"
	magnet := "magnet:?xt=urn:btih:" + hash
	if _, err := uc.AddMagnetWithPolicy(magnet, TorrentMetadata{}, true); err == nil {
		t.Fatal("Add hid committed apply failure")
	}
	i, err := repo.GetIntent(hash)
	if err != nil || i == nil {
		t.Fatal("lost committed Add", i, err)
	}
	model, err := uc.GetTorrent(hash)
	if err != nil || !model.Control.ReconcilePending || model.Control.ApplyFailure == nil || model.Control.ApplyFailure.Code != "runtime_bind_failed" {
		t.Fatal(model, err)
	}
	if uc.engine.GetRawTorrent(hash).DataDownloadAllowed() {
		t.Fatal("failed Add was admitted")
	}
	if _, err := db.DB().Exec(`DROP TRIGGER reject_add_bind`); err != nil {
		t.Fatal(err)
	}
	model, err = uc.AddMagnetWithPolicy(magnet, TorrentMetadata{}, true)
	if err != nil || !model.Control.ReconcilePending || model.Control.ApplyFailure == nil {
		t.Fatal("duplicate Add acknowledged failed apply", model, err)
	}
	if uc.engine.GetRawTorrent(hash).DataDownloadAllowed() {
		t.Fatal("duplicate Add reopened gate")
	}
	if err := uc.Resume(hash); err != nil {
		t.Fatal(err)
	}
	model, err = uc.GetTorrent(hash)
	if err != nil || model.Control.ReconcilePending || model.Control.ApplyFailure != nil {
		t.Fatal(model, err)
	}
}
