package usecase

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"
)

func TestStorageEnrollmentPreservesInterruptedUpgradeJournal(t *testing.T) {
	for _, mode := range []string{"deletion", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			uc, _, closeFirst := setupTorrentUseCase(t)
			tor, m := moveFixture(t, uc, false, []byte("retained"))
			dir := uc.engine.DownloadDir()
			closeFirst()
			db, err := repository.NewSQLiteDB(filepath.Join(dir, "hub.db"))
			if err != nil {
				t.Fatal(err)
			}
			repo := repository.NewTorrentRepo(db)
			if mode == "deletion" {
				if err := repo.BeginCleanup(tor.Hash, []string{filepath.Join(m.Root, "video.bin")}); err != nil {
					t.Fatal(err)
				}
				if _, err := db.DB().Exec(`UPDATE storage_recovery SET stage='cancel_requested' WHERE hash=?`, tor.Hash); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := db.DB().Exec(`INSERT INTO storage_recovery(hash,record_id,version,reason,stage,journal) VALUES(?,?,3,'legacy_ambiguous','preserved','{}')`, tor.Hash, tor.Control.RecordID); err != nil {
					t.Fatal(err)
				}
			}
			before, err := repo.Recovery(tor.Hash)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.DB().Exec(`DELETE FROM storage_volumes`); err != nil {
				t.Fatal(err)
			}
			db.Close()
			if err := os.Remove(filepath.Join(dir, payload.VolumeMarker)); err != nil {
				t.Fatal(err)
			}
			restored, done := reopenInventoryFixture(t, dir)
			defer done()
			cmd := models.StorageCommand{Action: "reconcile_storage", RecordID: tor.Control.RecordID, StorageID: m.StorageID, MappingRevision: m.Revision}
			if _, err := restored.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
				t.Fatal("journal prevented explicit enrollment", err)
			}
			if err := restored.checkVolume(m.Root); err != nil {
				t.Fatal(err)
			}
			after, err := restored.repo.Recovery(tor.Hash)
			if err != nil || *after != *before {
				t.Fatal("enrollment rewrote interrupted operation", before, after, err)
			}
			current, err := restored.repo.GetTorrent(tor.Hash)
			if err != nil || current.Control.DesiredRun != models.RunPaused || mode == "deletion" && current.DeletionMode != "payload" {
				t.Fatal("enrollment lifted intent/block", err)
			}
			if raw := restored.engine.GetRawTorrent(tor.Hash); raw != nil {
				t.Fatal("enrollment attached blocked payload")
			}
			if data, err := os.ReadFile(filepath.Join(dir, m.Root, "video.bin")); err != nil || string(data) != "retained" {
				t.Fatal("enrollment changed payload", err)
			}
		})
	}
}
