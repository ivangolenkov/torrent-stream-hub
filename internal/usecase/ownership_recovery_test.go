package usecase

import (
	"context"
	"errors"
	"os"
	"testing"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"
)

func TestOwnershipReservationProtectsPublication(t *testing.T) {
	uc, _, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	root, err := os.OpenRoot(uc.engine.DownloadDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	release, err := uc.reserveOwnership(context.Background(), root, "one", []string{payload.Root(testInfoHash)})
	if err != nil {
		t.Fatal(err)
	}
	m := &repository.StorageMapping{Hash: "two", Root: payload.Root(testInfoHash), State: "ready", Version: payload.LayoutVersion}
	if err := uc.publishStorage(m, false, true); !errors.Is(err, ErrStorageConflict) {
		t.Fatalf("publication crossed preflight reservation: %v", err)
	}
	release()
	if err := uc.publishStorage(m, false, true); err != nil {
		t.Fatal(err)
	}
}

func TestOwnershipExistingAliasCannotAttachDuringRecovery(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	for _, hash := range []string{"owner", "alias"} {
		if err := repo.SaveStorageMapping(&repository.StorageMapping{Hash: hash, Root: payload.Root(testInfoHash), State: "ready", Version: payload.LayoutVersion}); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(uc.engine.DownloadDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	release, err := uc.reserveOwnership(context.Background(), root, "owner", []string{payload.Root(testInfoHash)})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := uc.reserveStorageLocked("alias", ""); !errors.Is(err, ErrStorageConflict) {
		t.Fatalf("existing alias bypassed reservation: %v", err)
	}
}

func TestOwnershipCatalogChurnIsBoundedAndCancelable(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	root, err := os.OpenRoot(uc.engine.DownloadDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	calls := 0
	uc.beforeOwnershipValidate = func() {
		calls++
		if err := repo.SaveStorageMapping(&repository.StorageMapping{Hash: "churn", Root: "other", State: string(rune('a' + calls))}); err != nil {
			t.Fatal(err)
		}
	}
	if err := uc.validateOwnership(root, "one", []string{"data"}, nil); !errors.Is(err, ErrOwnershipCatalogChanged) {
		t.Fatalf("churn not diagnosed: %v", err)
	}
	if calls != 16 {
		t.Fatalf("unbounded retry count %d", calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := uc.validateOwnershipContext(ctx, root, "one", []string{"data"}, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestOwnershipKeptLegacyOrphanStillReservesSources(t *testing.T) {
	uc, _, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor, _ := legacyMigrationFixture(t, uc, migrationMetainfo(t, "kept", []byte("remaining"), false, "v1"), []byte("remaining"), false)
	if err := uc.Delete(tor.Hash, false); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(uc.engine.DownloadDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := uc.reserveOwnership(context.Background(), root, "another", []string{"kept"}); !errors.Is(err, ErrStorageConflict) {
		t.Fatalf("kept source became unowned: %v", err)
	}
	release, err := uc.reserveOwnership(context.Background(), root, "another", []string{payload.Root(testInfoHash)})
	if err != nil {
		t.Fatal("unrelated isolated namespace blocked", err)
	}
	release()
}
