package usecase

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
)

func TestRestoreActivationUsesTypedKeysNotReasonOrQueryOrder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		run    models.RunIntent
		disk   string
		blocks []models.ActivationBlock
		state  models.TorrentState
		err    models.ErrorReason
	}{
		{name: "enabled", run: models.RunEnabled, state: models.StateQueued},
		{name: "invalid intent fails closed", run: "unexpected", state: models.StateError, err: models.ErrActivationBlocked},
		{name: "verification owns its error", run: models.RunEnabled, blocks: []models.ActivationBlock{{Kind: models.BlockVerification, Reason: "arbitrary diagnostic"}}, state: models.StateError, err: models.ErrVerificationFailed},
		{name: "missing files is not generic Error", run: models.RunEnabled, blocks: []models.ActivationBlock{{Kind: models.BlockMissingFiles, Reason: "old file missing"}}, state: models.StateMissingFiles, err: models.ErrMissingFiles},
		{name: "legacy text does not grant verification ownership", run: models.RunEnabled, blocks: []models.ActivationBlock{{Kind: models.BlockLegacyError, Reason: string(models.ErrVerificationFailed)}}, state: models.StateError, err: models.ErrActivationBlocked},
		{name: "foreign key wins over verification", run: models.RunEnabled, blocks: []models.ActivationBlock{{Kind: models.BlockVerification}, {Kind: models.BlockMissingFiles}}, state: models.StateMissingFiles, err: models.ErrMissingFiles},
		{name: "unknown key wins over verification", run: models.RunEnabled, blocks: []models.ActivationBlock{{Kind: models.BlockVerification}, {Kind: "future"}}, state: models.StateError, err: models.ErrActivationBlocked},
		{name: "disk display", run: models.RunEnabled, disk: "sticky", blocks: []models.ActivationBlock{{Kind: models.BlockVerification}}, state: models.StateDiskFull, err: "sticky"},
		{name: "pause dominates every display", run: models.RunPaused, disk: "sticky", blocks: []models.ActivationBlock{{Kind: models.BlockVerification}, {Kind: models.BlockMissingFiles}, {Kind: models.BlockLegacyError}}, state: models.StatePaused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, reason := restoreActivation(tc.run, tc.disk, tc.blocks)
			if state != tc.state || reason != tc.err {
				t.Fatal(state, reason)
			}
			// Ordering of independently owned rows is not state precedence.
			reversed := append([]models.ActivationBlock(nil), tc.blocks...)
			for a, b := 0, len(reversed)-1; a < b; a, b = a+1, b-1 {
				reversed[a], reversed[b] = reversed[b], reversed[a]
			}
			if s, e := restoreActivation(tc.run, tc.disk, reversed); s != state || e != reason {
				t.Fatal("order-dependent projection", s, e)
			}
		})
	}
}

func TestActivationBlockCombinationsSurviveRealReopenAndVerification(t *testing.T) {
	for _, tc := range []struct {
		name          string
		run           models.RunIntent
		disk          bool
		foreign       models.ActivationBlockKind
		before, after models.TorrentState
	}{
		{name: "paused verification", run: models.RunPaused, before: models.StatePaused, after: models.StatePaused},
		{name: "enabled verification", run: models.RunEnabled, before: models.StateError, after: models.StateSeeding},
		{name: "enabled missing and verification", run: models.RunEnabled, foreign: models.BlockMissingFiles, before: models.StateMissingFiles, after: models.StateMissingFiles},
		{name: "enabled legacy and verification", run: models.RunEnabled, foreign: models.BlockLegacyError, before: models.StateError, after: models.StateError},
		{name: "enabled metadata and verification", run: models.RunEnabled, foreign: models.BlockMetadataPersistence, before: models.StateError, after: models.StateError},
		{name: "enabled selection and verification", run: models.RunEnabled, foreign: models.BlockInvalidSelection, before: models.StateError, after: models.StateError},
		{name: "enabled disk and verification", run: models.RunEnabled, disk: true, before: models.StateDiskFull, after: models.StateDiskFull},
		{name: "paused disk missing verification", run: models.RunPaused, disk: true, foreign: models.BlockMissingFiles, before: models.StatePaused, after: models.StatePaused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uc, repo, cleanup := setupTorrentUseCase(t)
			defer cleanup()
			tor, _ := completedSafetyFile(t, uc, "activation-reopen", false)
			if err := uc.Pause(tor.Hash); err != nil {
				t.Fatal(err)
			}
			if tc.run == models.RunEnabled {
				if err := uc.Resume(tor.Hash); err != nil {
					t.Fatal(err)
				}
			}
			i, err := repo.GetIntent(tor.Hash)
			if err != nil {
				t.Fatal(err)
			}
			_, f := uc.engine.RuntimeBinding(tor.Hash)
			if err := repo.PutActivationBlock(f, models.BlockVerification, "retained verifier diagnostic"); err != nil {
				t.Fatal(err)
			}
			// Deliberately collide with the verification display string: reason
			// text must never grant authority over the legacy key.
			if tc.foreign != "" {
				if err := repo.PutActivationBlock(f, tc.foreign, string(models.ErrVerificationFailed)); err != nil {
					t.Fatal(err)
				}
			}
			if tc.disk {
				if err := repo.SaveDownloadBlock(tor.Hash, "sticky disk latch"); err != nil {
					t.Fatal(err)
				}
			}
			dir := uc.engine.DownloadDir()
			cleanup()
			db, err := repository.NewSQLiteDB(filepath.Join(dir, "hub.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			e, err := engine.New(&config.Config{DownloadDir: dir, DBPath: filepath.Join(dir, "hub.db"), BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			r := repository.NewTorrentRepo(db)
			restored := NewTorrentUseCase(e, r)
			// Before attachment, REST and SSE must use durable state projection,
			// not the pre-close operational snapshot or SQL row ordering.
			detached, err := restored.GetTorrent(tor.Hash)
			if err != nil || detached.State != tc.before || detached.Control.ReconcileReason != "runtime_not_attached" {
				t.Fatal("DB-only REST lost activation authorities", detached, err)
			}
			sw := NewSyncWorker(restored)
			defer sw.Close()
			ch := make(chan []byte, 1)
			sw.AddClient(ch)
			sw.Refresh()
			var snapshot []*models.Torrent
			var message []byte
			select {
			case message = <-ch:
			case <-time.After(3 * time.Second):
				t.Fatal("DB-only SSE did not publish")
			}
			if err := json.Unmarshal(message, &snapshot); err != nil || len(snapshot) != 1 || snapshot[0].State != tc.before {
				t.Fatal("DB-only SSE disagrees with restore/REST", snapshot, err)
			}
			sw.RemoveClient(ch)
			sw.Close()
			if err := restored.RestoreTorrents(); err != nil {
				t.Fatal(err)
			}
			view, err := restored.GetTorrent(tor.Hash)
			if err != nil || view.State != tc.before {
				t.Fatal("lost restore precedence", view, err)
			}
			raw := e.GetRawTorrent(tor.Hash)
			if raw == nil || raw.NetworkingEnabled() || raw.DataDownloadAllowed() {
				t.Fatal("blocked reopen activated networking/payload")
			}
			if err := restored.Resume(tor.Hash); err == nil {
				t.Fatal("Resume bypassed verification key")
			}
			if _, err := restored.Recheck(tor.Hash); err != nil {
				t.Fatal(err)
			}
			// This only awaits a real async verifier's result. The controlled
			// stale-completion interleavings are tested separately in engine.
			deadline := time.After(3 * time.Second)
			for {
				view, err = restored.GetTorrent(tor.Hash)
				if err != nil && !errors.Is(err, ErrSnapshotConflict) {
					t.Fatal(err)
				}
				if err == nil && view.Recheck != nil && view.Recheck.Status == "succeeded" && view.State == tc.after {
					break
				}
				select {
				case <-deadline:
					t.Fatal("verification failed to project current authorities", view)
				default:
					time.Sleep(time.Millisecond)
				}
			}
			if raw.NetworkingEnabled() {
				t.Fatal("local verification enabled networking")
			}
			after, err := r.GetIntent(tor.Hash)
			if err != nil || *after != *i {
				t.Fatal("verification changed intent", after, err)
			}
			blocks, err := r.ActivationBlocks(i.Version)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.foreign != "" {
				want = 1
			}
			if len(blocks) != want || want == 1 && (blocks[0].Kind != tc.foreign || blocks[0].Reason != string(models.ErrVerificationFailed)) {
				t.Fatal("verification cleared/mutated foreign key", blocks)
			}
			if disk, err := r.DownloadBlock(tor.Hash); err != nil || tc.disk && disk != "sticky disk latch" || !tc.disk && disk != "" {
				t.Fatal("verification changed disk latch", disk, err)
			}
		})
	}
}
