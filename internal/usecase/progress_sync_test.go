package usecase

import (
	"bytes"
	"context"
	"os"
	"testing"
)

func TestProgressSyncCannotRestoreBytesAfterManagedRecheck(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor, path := completedSafetyFile(t, uc, "sync-replaced", true)
	sw := NewSyncWorker(uc)
	defer sw.Close()
	persistForTest(t, uc, tor.Hash)
	sw.Refresh()
	stale, err := uc.GetTorrent(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if stale.Progress != 100 || stale.ProgressSource != "verified" {
		t.Fatal("fixture not verified")
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("X"), 1024), 0600); err != nil {
		t.Fatal(err)
	}
	// Managed payload no longer promises mutation detection during polling.
	// Explicit Recheck revokes trust; a stale DB snapshot must not restore it.
	if err := uc.engine.GetRawTorrent(tor.Hash).VerifyDataContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	persistForTest(t, uc, tor.Hash)
	sw.Refresh()
	if err := repo.SaveTorrent(stale); err != nil {
		t.Fatal(err)
	}
	saved, err := repo.GetTorrent(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Downloaded != 0 || saved.Files[0].Downloaded != 0 || saved.ProgressSource != "last_known" {
		t.Fatal("stale snapshot restored verified bytes", saved)
	}
}
