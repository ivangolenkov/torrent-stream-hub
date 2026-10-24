package usecase

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
)

// Persistence assertions use an explicit barrier. Refresh only publishes and
// schedules observations; its return no longer promises a database commit.
func persistForTest(t *testing.T, uc *TorrentUseCase, hash string) {
	t.Helper()
	release, err := uc.lockRecord(hash)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := capturePersistableRuntime(uc.engine, uc.repo, hash, context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPublisherDoesNotWaitForRecordOrSQLiteWriter(t *testing.T) {
	uc, _, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	a := temporaryFile(t, uc, "publisher-A", false)
	b := temporaryFile(t, uc, "publisher-B", false)
	sw := NewSyncWorker(uc)
	defer sw.Close()
	ch := make(chan PublishedSnapshot, 1)
	sw.AddSnapshotClient(ch)
	release, err := uc.lockRecord(a.Hash)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	db, err := repository.NewSQLiteDB(filepath.Join(uc.engine.DownloadDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := uc.engine.Pause(b.Hash); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { sw.Refresh(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publication waited for persistence")
	}
	var list []*models.Torrent
	if err := json.Unmarshal((<-ch).Data, &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range list {
		if item.Hash == b.Hash {
			found = true
			if item.State != models.StatePaused || item.SnapshotFreshness != "sampled" {
				t.Fatal(item)
			}
		}
	}
	if !found {
		t.Fatal("independent torrent disappeared")
	}
}

func TestCompactPublicationDoesNotContainCatalog(t *testing.T) {
	tor := &models.Torrent{Hash: "hash", CatalogVersion: "v1"}
	for i := range 10000 {
		tor.Files = append(tor.Files, &models.File{Index: i, Path: "file"})
	}
	data, err := json.Marshal(CompactTorrents([]*models.Torrent{tor}))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 1500 {
		t.Fatalf("compact retained catalog: %d", len(data))
	}
	if len(tor.Files) != 10000 {
		t.Fatal("projection mutated source")
	}
}
