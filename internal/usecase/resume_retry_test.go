package usecase

import (
	"testing"
	"torrent-stream-hub/internal/models"
)

func TestExplicitResumeRetriesLegacyAndMissingBlocksWithoutForgingReadiness(t *testing.T) {
	for _, kind := range []models.ActivationBlockKind{models.BlockLegacyError, models.BlockMissingFiles, models.BlockMetadataPersistence, models.BlockInvalidSelection} {
		t.Run(string(kind), func(t *testing.T) {
			uc, repo, cleanup := setupTorrentUseCase(t)
			defer cleanup()
			tor := temporaryFile(t, uc, "resume-retry", false)
			if err := uc.Pause(tor.Hash); err != nil {
				t.Fatal(err)
			}
			_, f := uc.engine.RuntimeBinding(tor.Hash)
			if err := repo.PutActivationBlock(f, kind, "legacy failure"); err != nil {
				t.Fatal(err)
			}
			if err := uc.engine.Delete(tor.Hash); err != nil {
				t.Fatal(err)
			}
			if err := uc.RestoreTorrents(); err != nil {
				t.Fatal(err)
			}
			if raw := uc.engine.GetRawTorrent(tor.Hash); raw == nil || raw.NetworkingEnabled() || raw.DataDownloadAllowed() {
				t.Fatal("restore retried without explicit permission")
			}
			if err := uc.Resume(tor.Hash); err != nil {
				t.Fatal(err)
			}
			i, err := repo.GetIntent(tor.Hash)
			if err != nil || i.Run != models.RunEnabled {
				t.Fatal(i, err)
			}
			blocks, err := repo.ActivationBlocks(i.Version)
			if err != nil || len(blocks) != 0 {
				t.Fatal(blocks, err)
			}
			model, err := uc.GetTorrent(tor.Hash)
			if err != nil || model.Control.ReconcilePending || model.Downloaded != 0 || model.State == models.StateSeeding {
				t.Fatal("retry invented verified payload", model, err)
			}
		})
	}
}
