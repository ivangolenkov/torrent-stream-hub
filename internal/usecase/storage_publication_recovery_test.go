package usecase

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"torrent-stream-hub/internal/models"
)

func TestStorageBlockedIOPublishesAndAllowsDeleteReaddOtherRecord(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	// This test drives the single migration worker explicitly below. Public Add
	// must not also launch a background pass over the newly seeded legacy A.
	uc.storageWorkerStarted = true
	data := bytes.Repeat([]byte("x"), 1<<20)
	tor, _ := legacyMigrationFixture(t, uc, migrationMetainfo(t, "blocked-io", data, false, "v1"), data, false)
	other, err := uc.AddMagnet("magnet:?xt=urn:btih:" + testInfoHash)
	if err != nil {
		t.Fatal(err)
	}
	before := other.Control.RecordID
	entered, proceed, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	var chunks atomic.Int64
	uc.storageByteHook = func(_ string, _ int64) {
		if chunks.Add(1) == 2 {
			close(entered)
			<-proceed
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	defer func() { cancel(); once.Do(func() { close(proceed) }); <-finished }()
	go func() { defer close(finished); result <- uc.migrateStorage(ctx, tor) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("did not enter blocked read")
	}
	uc.storageLiveMu.Lock()
	uc.storageLive[tor.Hash].progress.LastProgressAt = 1
	uc.storageLiveMu.Unlock()
	worker := NewSyncWorker(uc)
	defer worker.Close()
	snapshots := make(chan PublishedSnapshot, 1)
	if !worker.AddSnapshotClient(snapshots) {
		t.Fatal("SSE subscription not admitted")
	}
	refreshed := make(chan struct{})
	go func() { worker.Refresh(); close(refreshed) }()
	select {
	case <-refreshed:
	case <-time.After(3 * time.Second):
		t.Fatal("publication waited for storage syscall")
	}
	var state []*models.Torrent
	select {
	case snapshot := <-snapshots:
		if err := json.Unmarshal(snapshot.Data, &state); err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("missing snapshot")
	}
	found := false
	for _, item := range state {
		if item.Hash == tor.Hash {
			found = true
			p := item.StorageRecovery.Progress
			if p.BytesDone != 128<<10 || p.HeartbeatAt <= p.LastProgressAt || p.LastProgressAt != 1 {
				t.Fatalf("wrong blocked-I/O progress: %+v", p)
			}
		}
	}
	if !found {
		t.Fatal("blocked record vanished from publication")
	}
	// These are real public commands during migration A's in-flight read, not
	// direct repository mutation. B's re-add gets a new record capability.
	if err := uc.Delete(other.Hash, false); err != nil {
		t.Fatal(err)
	}
	replacement, err := uc.AddMagnet("magnet:?xt=urn:btih:" + testInfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Control.RecordID == before {
		t.Fatal("re-add reused identity")
	}
	if err := uc.Pause(replacement.Hash); err != nil {
		t.Fatal("unrelated command waited or failed", err)
	}
	cancel()
	once.Do(func() { close(proceed) })
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	mapping, err := repo.StorageMapping(tor.Hash)
	if err != nil || mapping.State == "blocked" {
		t.Fatalf("catalog mutation falsely blocked A: %+v %v", mapping, err)
	}
}
