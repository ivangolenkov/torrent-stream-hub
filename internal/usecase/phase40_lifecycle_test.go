package usecase

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestHEADLeaseProtectsDuringRequestWithoutPlaybackDebounce(t *testing.T) {
	uc, _, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor := temporaryMagnet(t, uc, 1)
	release, err := uc.AcquireHTTPTransfer(tor.Hash, context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	uc.policyMu.Lock()
	active := uc.transfers[tor.Hash]
	uc.policyMu.Unlock()
	if active != 1 {
		t.Fatal("HEAD did not protect record")
	}
	release()
	release()
	uc.policyMu.Lock()
	active = uc.transfers[tor.Hash]
	until := uc.protectedUntil[tor.Hash]
	uc.policyMu.Unlock()
	if active != 0 || !until.IsZero() {
		t.Fatal("HEAD left playback accounting", active, until)
	}
	if err := uc.Delete(tor.Hash, false); err != nil {
		t.Fatal("HEAD blocked delete", err)
	}
}

func TestSyncCaptureCancelledWhileRecordReserved(t *testing.T) {
	uc, _, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor := temporaryMagnet(t, uc, 1)
	release, err := uc.lockRecord(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	sw := NewSyncWorker(uc)
	defer sw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := sw.captureSnapshotContext(ctx); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("capture ignored cancellation")
	}
}

func TestDownloadedFilePreparationUsesRequestCancellation(t *testing.T) {
	uc, _, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor := temporaryFile(t, uc, "cancel-download", false)
	if err := uc.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	uc.inactiveVerify = func(ctx context.Context, _ string) error { close(entered); <-ctx.Done(); return ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := uc.GetDownloadedFile(tor.Hash, 0, ctx); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("preparation did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("preparation ignored cancellation")
	}
}
