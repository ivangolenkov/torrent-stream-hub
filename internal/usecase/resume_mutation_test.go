package usecase

import (
	"bytes"
	"context"
	"errors"
	"github.com/anacrolix/torrent/metainfo"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"torrent-stream-hub/internal/fastresume"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
)

func sealManagedResume(t *testing.T, uc *TorrentUseCase, hash string) (*repository.StorageMapping, repository.ResumeSource) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, err := uc.lockRecord(hash, ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	saved, err := uc.repo.GetTorrent(hash)
	if err != nil {
		t.Fatal(err)
	}
	if uc.engine.GetRawTorrent(hash) == nil {
		if err := uc.restoreInactiveLocked(saved); err != nil {
			t.Fatal(err)
		}
	}
	raw := uc.engine.GetRawTorrent(hash)
	if raw == nil {
		t.Fatal("fixture runtime missing")
	}
	if err := raw.VerifyDataContext(ctx); err != nil {
		t.Fatal(err)
	}
	m, err := uc.repo.StorageMapping(hash)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := uc.repo.GetIntent(hash)
	if err != nil {
		t.Fatal(err)
	}
	mi := raw.Metainfo()
	uc.volumeMu.RLock()
	volume := uc.volumes[0].Token
	uc.volumeMu.RUnlock()
	source := repository.ResumeSource{Identity: fastresume.Identity{RecordID: intent.Version.RecordID, StorageID: m.StorageID, MappingRevision: m.Revision, LayoutVersion: m.Version, Root: m.Root, Prefix: m.Prefix, VolumeID: volume}, Metadata: &mi, Info: raw.Info(), Capture: raw.WithFileCheckpoint, Authorize: func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if uc.engine.GetRawTorrent(hash) != raw {
			return ErrStorageConflict
		}
		return uc.checkVolume(m.Root)
	}}
	if err := uc.repo.SealResume(ctx, m, source); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := uc.repo.LoadResume(ctx, m, intent.Version.RecordID)
	if err != nil || checkpoint == nil || checkpoint.Invalidated {
		t.Fatal("fixture checkpoint not committed", checkpoint, err)
	}
	for _, b := range checkpoint.Checkpoint.Pieces {
		if b != 0 {
			return m, source
		}
	}
	t.Fatal("fixture did not seal verified pieces")
	return nil, source
}

func failResumeMutationSQL(t *testing.T, uc *TorrentUseCase) func() {
	t.Helper()
	db, err := repository.NewSQLiteDB(filepath.Join(uc.engine.DownloadDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`CREATE TRIGGER fail_application_resume BEFORE UPDATE OF mutation_epoch ON resume_owners BEGIN SELECT RAISE(ABORT,'resume mutation blocked'); END`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	return func() { _, _ = db.DB().Exec(`DROP TRIGGER fail_application_resume`); _ = db.Close() }
}

func TestResumeApplicationFailedBarrierPreventsEffects(t *testing.T) {
	for _, action := range []string{"recheck", "delete", "reconcile", "move", "migration", "repair"} {
		t.Run(action, func(t *testing.T) {
			uc, repo, done := setupTorrentUseCase(t)
			defer done()
			data := bytes.Repeat([]byte("good"), 8192)
			tor, _ := moveFixture(t, uc, false, data)
			m, source := sealManagedResume(t, uc, tor.Hash)
			var move models.StorageCommand
			if action == "move" {
				events := locationEvents(uc)
				current, _ := repo.GetTorrent(tor.Hash)
				move = planMoveFixture(t, uc, current, m, events)
			}
			if action == "migration" {
				m.State = "legacy"
				if err := repo.SaveStorageMapping(m); err != nil {
					t.Fatal(err)
				}
			}
			if action == "repair" {
				if err := repo.QueueInventory(m, source.Identity.RecordID, uc.inventorySession); err != nil {
					t.Fatal(err)
				}
				if ready, err := uc.inspectInventory(context.Background(), tor.Hash, false); err != nil || !ready {
					t.Fatal(ready, err)
				}
				if err := os.Remove(filepath.Join(uc.engine.DownloadDir(), m.Root, "video.bin")); err != nil {
					t.Fatal(err)
				}
				if err := repo.QueueInventory(m, source.Identity.RecordID, uc.inventorySession); err != nil {
					t.Fatal(err)
				}
				if ready, err := uc.inspectInventory(context.Background(), tor.Hash, false); err != nil || ready {
					t.Fatal(ready, err)
				}
			}
			undo := failResumeMutationSQL(t, uc)
			defer undo()
			var err error
			switch action {
			case "recheck":
				_, err = uc.Recheck(tor.Hash)
			case "delete":
				err = uc.Delete(tor.Hash, true)
			case "reconcile":
				_, err = uc.StorageCommand(context.Background(), tor.Hash, models.StorageCommand{Action: "reconcile_storage", RecordID: source.Identity.RecordID, StorageID: m.StorageID, MappingRevision: m.Revision})
			case "move":
				_, err = uc.StorageCommand(context.Background(), tor.Hash, move)
			case "migration":
				current, _ := repo.GetTorrent(tor.Hash)
				err = uc.migrateStorage(context.Background(), current)
			case "repair":
				err = uc.Resume(tor.Hash)
			}
			if err == nil || !strings.Contains(err.Error(), "resume mutation blocked") {
				t.Fatal("workflow did not stop at durable barrier", err)
			}
			actual, err := os.ReadFile(filepath.Join(uc.engine.DownloadDir(), m.Root, "video.bin"))
			if action == "repair" {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed repair recreated payload", err)
				}
			} else if err != nil || !bytes.Equal(actual, data) {
				t.Fatal("payload changed across failed barrier", err)
			}
			if _, err := os.Stat(filepath.Join(uc.engine.DownloadDir(), "payload", "Moved")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed mutation published target", err)
			}
		})
	}
}

func TestResumeLegacyFailedBarrierPreservesBothCandidates(t *testing.T) {
	uc, repo, done := setupTorrentUseCase(t)
	defer done()
	hash := legacyConflictFixture(t, uc, "v1", []byte("damaged!"), []byte("original"))
	before, err := repo.Recovery(hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := uc.analyzeLegacy(context.Background(), hash, before.RecordID, before.Version); err != nil {
		t.Fatal(err)
	}
	rec, err := repo.Recovery(hash)
	if err != nil {
		t.Fatal(err)
	}
	j, err := decodeLegacyJournal(rec)
	if err != nil {
		t.Fatal(err)
	}
	m, err := repo.StorageMapping(hash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CaptureResume(context.Background(), m, rec.RecordID); err != nil {
		t.Fatal("fixture pending seal fence", err)
	}
	undo := failResumeMutationSQL(t, uc)
	defer undo()
	err = uc.applyLegacy(context.Background(), hash, rec.RecordID, rec.Version, "part")
	if err == nil || !strings.Contains(err.Error(), "resume mutation blocked") {
		t.Fatal("legacy publication did not stop at barrier", err)
	}
	for suffix, want := range map[string]string{"": "damaged!", ".part": "original"} {
		data, err := os.ReadFile(filepath.Join(uc.engine.DownloadDir(), m.Root, j.MetadataPath) + suffix)
		if err != nil || string(data) != want {
			t.Fatal("failed barrier changed candidate", suffix, string(data), err)
		}
	}
}

func assertManagedResumeRejected(t *testing.T, uc *TorrentUseCase, m *repository.StorageMapping, source repository.ResumeSource) {
	t.Helper()
	s, err := uc.repo.LoadResume(context.Background(), m, source.Identity.RecordID)
	if err == nil && s != nil && !s.Invalidated {
		t.Fatal("mutation preserved old checkpoint authority", s.Token)
	}
	if err != nil && !errors.Is(err, repository.ErrStateConflict) {
		t.Fatal(err)
	}
}

func TestResumeApplicationMutationMatrix(t *testing.T) {
	for _, action := range []string{"recheck", "reconcile", "delete-keep", "delete-payload", "move", "locate", "repair"} {
		t.Run(action, func(t *testing.T) {
			uc, repo, done := setupTorrentUseCase(t)
			defer done()
			data := bytes.Repeat([]byte("good"), 8192)
			tor, m := moveFixture(t, uc, false, data)
			m, source := sealManagedResume(t, uc, tor.Hash)
			switch action {
			case "recheck":
				if _, err := uc.Recheck(tor.Hash); err != nil {
					t.Fatal(err)
				}
			case "reconcile":
				cmd := models.StorageCommand{Action: "reconcile_storage", RecordID: source.Identity.RecordID, StorageID: m.StorageID, MappingRevision: m.Revision}
				if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
					t.Fatal(err)
				}
			case "delete-keep", "delete-payload":
				if err := uc.Delete(tor.Hash, action == "delete-payload"); err != nil {
					t.Fatal(err)
				}
				var input bytes.Buffer
				if err := source.Metadata.Write(&input); err != nil {
					t.Fatal(err)
				}
				added, err := uc.AddTorrentFile(&input)
				if err != nil {
					t.Fatal(err)
				}
				if added.Control.RecordID == source.Identity.RecordID {
					t.Fatal("re-add inherited record identity")
				}
			case "move":
				events := locationEvents(uc)
				current, err := repo.GetTorrent(tor.Hash)
				if err != nil {
					t.Fatal(err)
				}
				cmd := planMoveFixture(t, uc, current, m, events)
				if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
					t.Fatal(err)
				}
				waitLocationEvent(t, uc, tor.Hash, events, "location_completed")
			case "locate":
				found := filepath.Join(uc.engine.DownloadDir(), "found")
				if err := os.MkdirAll(found, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(found, "video.bin"), data, 0600); err != nil {
					t.Fatal(err)
				}
				events := locationEvents(uc)
				cmd := models.StorageCommand{Action: "analyze_location", RecordID: source.Identity.RecordID, StorageID: m.StorageID, MappingRevision: m.Revision, Path: "found", StripPrefix: "Original"}
				if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
					t.Fatal(err)
				}
				waitLocationEvent(t, uc, tor.Hash, events, "location_analyzed")
				rec, err := repo.Recovery(tor.Hash)
				if err != nil {
					t.Fatal(err)
				}
				cmd.Action, cmd.Version = "apply_location", rec.Version
				if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
					t.Fatal(err)
				}
				waitLocationEvent(t, uc, tor.Hash, events, "location_completed")
			case "repair":
				if err := repo.QueueInventory(m, source.Identity.RecordID, uc.inventorySession); err != nil {
					t.Fatal(err)
				}
				if ready, err := uc.inspectInventory(context.Background(), tor.Hash, false); err != nil || !ready {
					t.Fatal("fixture did not enroll present file evidence", ready, err)
				}
				if err := os.Remove(filepath.Join(uc.engine.DownloadDir(), m.Root, "video.bin")); err != nil {
					t.Fatal(err)
				}
				if err := repo.QueueInventory(m, source.Identity.RecordID, uc.inventorySession); err != nil {
					t.Fatal(err)
				}
				if ready, err := uc.inspectInventory(context.Background(), tor.Hash, false); err != nil || ready {
					t.Fatal("fixture did not detect missing expected file", ready, err)
				}
				if err := uc.Resume(tor.Hash); err != nil {
					t.Fatal(err)
				}
			}
			assertManagedResumeRejected(t, uc, m, source)
		})
	}
}

func TestResumeSelectionAndPausePreserveIndependentTrust(t *testing.T) {
	uc, repo, done := setupTorrentUseCase(t)
	defer done()
	tor, _ := moveFixture(t, uc, false, bytes.Repeat([]byte("good"), 8192))
	m, source := sealManagedResume(t, uc, tor.Hash)
	before, err := repo.LoadResume(context.Background(), m, source.Identity.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	if err := uc.SetFilePriority(tor.Hash, 0, models.PriorityNone); err != nil {
		t.Fatal(err)
	}
	if err := uc.Resume(tor.Hash); err != nil {
		t.Fatal(err)
	}
	if err := uc.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	after, err := repo.LoadResume(context.Background(), m, source.Identity.RecordID)
	if err != nil || after.Invalidated || after.Token != before.Token || !bytes.Equal(after.Revoked, before.Revoked) {
		t.Fatal("selection/Pause revoked valid content", after, err)
	}
}

func TestResumeMutationAliasesShareOwnerAndRejectSplitLayout(t *testing.T) {
	uc, repo, done := setupTorrentUseCase(t)
	defer done()
	tor, _ := moveFixture(t, uc, false, bytes.Repeat([]byte("good"), 8192))
	m, source := sealManagedResume(t, uc, tor.Hash)
	alias := metainfo.Hash{0: 1}
	if err := repo.ClaimStorageAlias(alias.HexString(), tor.Hash); err != nil {
		t.Fatal(err)
	}
	hooks, err := uc.resumeMutationHooks(context.Background(), source.Info, alias)
	if err != nil || hooks == nil {
		t.Fatal("consistent alias rejected", err)
	}
	assertManagedResumeRejected(t, uc, m, source)
	current, record, err := repo.ResumeOwnerMapping(context.Background(), m.StorageID)
	if err != nil || current.Hash != tor.Hash || record != source.Identity.RecordID {
		t.Fatal("alias acquired independent resume owner", current, record, err)
	}
	split, err := repo.StorageMapping(alias.HexString())
	if err != nil {
		t.Fatal(err)
	}
	split.Root = filepath.Join("payload", "split")
	if err := repo.SaveStorageMapping(split); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.resumeMutationHooks(context.Background(), source.Info, alias); !errors.Is(err, ErrStorageConflict) {
		t.Fatal("split alias layout granted a mutation capability", err)
	}
}
