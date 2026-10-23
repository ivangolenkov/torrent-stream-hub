package usecase

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"torrent-stream-hub/internal/models"
)

func independentCommandAndLease(t *testing.T, uc *TorrentUseCase, hash string, unblock func()) {
	t.Helper()
	result := make(chan error, 1)
	go func() {
		if err := uc.Pause(hash); err != nil {
			result <- err
			return
		}
		release, err := uc.AcquireTransfer(hash)
		if err == nil {
			release()
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		unblock()
		t.Fatal("B command/lease waited for A's long operation")
	}
}

func TestCoordinatorHashReadIndependenceAndCancelledWaiter(t *testing.T) {
	uc, _, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	a := temporaryFile(t, uc, "coordinator-hash-A", false)
	b := temporaryFile(t, uc, "coordinator-hash-B", false)
	if err := uc.Pause(a.Hash); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	uc.inactiveVerify = func(ctx context.Context, hash string) error {
		close(entered)
		<-release
		return uc.engine.VerifyInactive(ctx, hash)
	}
	finished := make(chan struct{})
	go func() {
		src, _ := uc.OpenStreamSource(context.Background(), a.Hash, 0, false)
		if src != nil {
			src.Close()
		}
		close(finished)
	}()
	<-entered
	independentCommandAndLease(t, uc, b.Hash, unblock)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := uc.OpenStreamSource(ctx, a.Hash, 0, false); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled same-record waiter admitted", err)
	}
	unblock()
	<-finished
}

func TestCoordinatorUnlinkIndependenceAndLeaseProtection(t *testing.T) {
	uc, _, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	a, _ := completedSafetyFile(t, uc, "coordinator-unlink-A", false)
	b := temporaryFile(t, uc, "coordinator-unlink-B", false)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var blocked atomic.Bool
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	uc.payloadRemove = func(root *os.Root, path string) error {
		if blocked.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return root.Remove(path)
	}
	finished := make(chan error, 1)
	go func() { finished <- uc.Delete(a.Hash, true) }()
	<-entered
	independentCommandAndLease(t, uc, b.Hash, unblock)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := uc.AcquireTransfer(a.Hash, ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled transfer bypassed reserved deletion", err)
	}
	if err := uc.MakePermanent(a.Hash, ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled permanence bypassed reserved deletion", err)
	}
	unblock()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorMigrationCopyIndependenceAndDurableConflict(t *testing.T) {
	uc, _, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	// Own this pass explicitly; don't race the fixture against an automatic pass.
	uc.storageInitMu.Lock()
	uc.storageWorkerStarted = true
	uc.storageInitMu.Unlock()
	b := temporaryFile(t, uc, "coordinator-copy-B", false)
	data := []byte("legacy copy data")
	a, _ := legacyMigrationFixture(t, uc, migrationMetainfo(t, "coordinator-copy-A", data, false, "v1"), data, false)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	uc.migrationCopy = func(ctx context.Context, root *os.Root, entry *migrationEntry) error {
		close(entered)
		<-release
		return copyMigrationEntry(ctx, root, entry)
	}
	finished := make(chan error, 1)
	go func() { finished <- uc.migrateStorage(context.Background(), a) }()
	<-entered
	independentCommandAndLease(t, uc, b.Hash, unblock)
	if err := uc.Resume(a.Hash); !errors.Is(err, ErrStorageConflict) {
		t.Fatal("moving journal admitted Resume", err)
	}
	if _, err := uc.AcquireTransfer(a.Hash); !errors.Is(err, ErrStorageConflict) {
		t.Fatal("moving journal admitted transfer", err)
	}
	unblock()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorOldLeaseReleaseCannotUnprotectReaddedRecord(t *testing.T) {
	uc, r, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor := temporaryFile(t, uc, "coordinator-lease-readd", false)
	oldRelease, err := uc.AcquireTransfer(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct an out-of-band delete/re-add to exercise the capability even
	// though normal application deletion correctly rejects an active lease.
	if err := r.DeleteTorrent(tor.Hash); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateTorrent(models.NewTorrentRecord{Hash: tor.Hash, Name: tor.Name, Run: models.RunPaused}); err != nil {
		t.Fatal(err)
	}
	newRelease, err := uc.AcquireTransfer(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	oldRelease()
	oldRelease()
	if count := uc.transferCount(tor.Hash); count != 1 {
		t.Fatal("old release consumed new record's lease", count)
	}
	if err := uc.Delete(tor.Hash, false); !errors.Is(err, ErrStorageConflict) {
		t.Fatal("new lease no longer protects deletion", err)
	}
	newRelease()
	newRelease()
	if count := uc.transferCount(tor.Hash); count != 0 {
		t.Fatal("lease counter leaked", count)
	}
}

func TestCoordinatorShutdownAfterDeletionCommitPreservesWorkflow(t *testing.T) {
	uc, r, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor, path := completedSafetyFile(t, uc, "coordinator-shutdown-delete", false)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	uc.beforeDetach = func(string) { close(entered); <-release }
	deleted := make(chan error, 1)
	go func() { deleted <- uc.Delete(tor.Hash, true) }()
	<-entered
	saved, err := r.GetTorrent(tor.Hash)
	if err != nil || saved.DeletionMode != "payload" {
		t.Fatal("work began before durable intent", saved, err)
	}
	closed := make(chan struct{})
	go func() { uc.engine.Close(); close(closed) }()
	<-uc.engine.Context().Done()
	select {
	case <-closed:
		t.Fatal("shutdown discarded active command handle")
	default:
	}
	unblock()
	if err := <-deleted; !errors.Is(err, context.Canceled) {
		t.Fatal("post-commit engine refusal was hidden", err)
	}
	<-closed
	saved, err = r.GetTorrent(tor.Hash)
	if err != nil || saved.DeletionMode != "payload" || saved.DeletionError == "" {
		t.Fatal("shutdown lost committed workflow", saved, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("engine refusal still unlinked payload", err)
	}
}

func TestCoordinatorWaitingOldRecordCommandRejectsReadd(t *testing.T) {
	uc, r, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor := temporaryFile(t, uc, "coordinator-waiting-record", false)
	owner, err := uc.lockRecord(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	defer owner()
	identities, err := uc.recordIdentities(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		release, err := uc.lockRecordsForIdentities(context.Background(), identities, tor.Hash)
		if release != nil {
			release()
		}
		result <- err
	}()
	// Model deletion and re-add under the current command owner. The waiting
	// command captured the old identity, irrespective of goroutine scheduling.
	if err := r.DeleteTorrent(tor.Hash); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateTorrent(models.NewTorrentRecord{Hash: tor.Hash, Name: tor.Name, Run: models.RunPaused}); err != nil {
		t.Fatal(err)
	}
	owner()
	if err := <-result; !errors.Is(err, ErrStorageConflict) {
		t.Fatal("old waiter acquired authority over a replacement record", err)
	}
}
