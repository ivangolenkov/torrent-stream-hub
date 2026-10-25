package usecase

import (
	"context"
	"errors"
	"os"
	"testing"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
)

func TestDeletionCancelJoinsOwnerAndPreservesRemainingData(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	data := []byte("keep")
	tor, source := legacyMigrationFixture(t, uc, migrationMetainfo(t, "cancel", data, false, "v1"), data, false)
	if err := os.WriteFile(source+".part", data, 0600); err != nil {
		t.Fatal(err)
	}
	entered, proceed := make(chan struct{}), make(chan struct{})
	calls := 0
	uc.payloadRemove = func(root *os.Root, path string) error {
		calls++
		err := root.Remove(path)
		if calls == 1 {
			close(entered)
			<-proceed
		}
		return err
	}
	done := make(chan error, 1)
	go func() { done <- uc.Delete(tor.Hash, true) }()
	<-entered
	rec, err := repo.Recovery(tor.Hash)
	if err != nil || rec == nil {
		t.Fatalf("missing operation: %v", err)
	}
	if err := repo.RequestDeletionCancel(tor.Hash, rec.RecordID, rec.Version); err != nil {
		t.Fatal(err)
	}
	close(proceed)
	if err := <-done; !errors.Is(err, repository.ErrDeletionCancelled) {
		t.Fatalf("old worker did not stop: %v", err)
	}
	if err := uc.CancelDeletion(context.Background(), tor.Hash, rec.RecordID, rec.Version); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("additional unlink: %d", calls)
	}
	if got, err := os.ReadFile(source + ".part"); err != nil || string(got) != "keep" {
		t.Fatalf("remaining data lost: %q %v", got, err)
	}
	saved, err := repo.GetTorrent(tor.Hash)
	if err != nil || saved == nil || saved.DeletionMode != "" || saved.Control.DesiredRun != models.RunPaused {
		t.Fatalf("bad cancellation state: %+v %v", saved, err)
	}
	if saved.StorageRecovery == nil || saved.StorageRecovery.Deletion.RemovedData != "partial" {
		t.Fatalf("partial deletion hidden: %+v", saved.StorageRecovery)
	}
	if err := uc.RetryDeletion(context.Background(), tor.Hash, rec.RecordID, rec.Version); !errors.Is(err, ErrStorageConflict) {
		t.Fatal("retry revived cancelled worker", err)
	}
	if err := uc.CancelDeletion(context.Background(), tor.Hash, rec.RecordID, rec.Version); err != nil {
		t.Fatal("cancel not idempotent", err)
	}
}

func TestDeletionCancelDisconnectedWaitRemainsDurable(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	data := []byte("keep")
	tor, _ := legacyMigrationFixture(t, uc, migrationMetainfo(t, "disconnect", data, false, "v1"), data, false)
	if err := repo.BeginCleanup(tor.Hash, []string{"disconnect"}); err != nil {
		t.Fatal(err)
	}
	rec, err := repo.Recovery(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := uc.CancelDeletion(ctx, tor.Hash, rec.RecordID, rec.Version); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	rec, err = repo.Recovery(tor.Hash)
	if err != nil || rec.Stage != "cancel_requested" {
		t.Fatal("lost durable cancellation", err)
	}
	if _, err := uc.restorePersistedTorrent(context.Background(), tor.Hash); err != nil {
		t.Fatal(err)
	}
	rec, err = repo.Recovery(tor.Hash)
	if err != nil || rec.Stage != "cancelled" {
		t.Fatal("restart did not finish cancellation", err)
	}
	if err := uc.ForgetRecovered(context.Background(), tor.Hash, rec.RecordID, rec.Version); err != nil {
		t.Fatal(err)
	}
	if err := uc.CancelDeletion(context.Background(), tor.Hash, rec.RecordID, rec.Version); !errors.Is(err, ErrStorageConflict) {
		t.Fatal("stale cancellation accepted", err)
	}
}

func TestDeletionCancelledLegacyWaitsForExplicitVerifiedResume(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	data := []byte("keep")
	tor, source := legacyMigrationFixture(t, uc, migrationMetainfo(t, "legacy-cancel", data, false, "v1"), data, false)
	if err := repo.BeginCleanup(tor.Hash, []string{"legacy-cancel"}); err != nil {
		t.Fatal(err)
	}
	rec, _ := repo.Recovery(tor.Hash)
	if err := uc.CancelDeletion(context.Background(), tor.Hash, rec.RecordID, rec.Version); err != nil {
		t.Fatal(err)
	}
	if err := uc.migrateStorage(context.Background(), tor); err != nil {
		t.Fatal(err)
	}
	mapping, _ := repo.StorageMapping(tor.Hash)
	if mapping.State != "legacy" || uc.engine.GetRawTorrent(tor.Hash) != nil {
		t.Fatal("cancelled legacy record auto-migrated or attached")
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal("cancelled source changed before explicit action", err)
	}
	if err := uc.resumeVerifiedRecovery(context.Background(), tor.Hash, rec.RecordID, rec.Version); err != nil {
		t.Fatal(err)
	}
	mapping, _ = repo.StorageMapping(tor.Hash)
	saved, _ := repo.GetTorrent(tor.Hash)
	if mapping.State != "ready" || saved.Control.DesiredRun != models.RunEnabled {
		t.Fatal("explicit verified resume did not finish layout recovery")
	}
}
