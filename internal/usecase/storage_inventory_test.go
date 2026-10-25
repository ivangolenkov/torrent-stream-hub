package usecase

import (
	"context"
	"errors"
	"fmt"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"os"
	"path/filepath"
	"testing"
	"time"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"
)

func TestInventoryVisitsBoundedCatalogPages(t *testing.T) {
	uc, repo, done := setupTorrentUseCase(t)
	defer done()
	info := metainfo.Info{Name: "many", PieceLength: 16384, Pieces: []byte{}}
	model := &models.Torrent{Name: "many", State: models.StatePaused}
	for i := 0; i < 257; i++ {
		name := fmt.Sprintf("file-%03d", i)
		info.Files = append(info.Files, metainfo.FileInfo{Path: []string{name}})
		model.Files = append(model.Files, &models.File{Index: i, Path: name})
	}
	data, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: data}
	model.Hash = mi.HashInfoBytes().HexString()
	meta := filepath.Join(uc.engine.ConfigDir(), "metainfo", model.Hash+".torrent")
	if err := os.MkdirAll(filepath.Dir(meta), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := mi.Write(f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := repo.SaveTorrent(model); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveStorageMapping(&repository.StorageMapping{Hash: model.Hash, Version: 2, Root: filepath.Join("payload", "many"), Prefix: "many", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	m, _ := repo.StorageMapping(model.Hash)
	for pass, want := range []int{127, 255, 256} {
		ready, err := uc.inspectInventory(context.Background(), model.Hash, false)
		if err != nil {
			t.Fatal(err)
		}
		if ready != (pass == 2) {
			t.Fatal("unbounded inventory", pass, ready)
		}
		state, _ := repo.Inventory(m.StorageID)
		if state.Cursor != want {
			t.Fatal("wrong page", state.Cursor, want)
		}
	}
	if _, err := os.Stat(filepath.Join(uc.engine.DownloadDir(), m.Root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("inventory created files", err)
	}
}

func TestInventoryUpgradeRequiresExplicitVolumeEnrollment(t *testing.T) {
	uc, _, closeFirst := setupTorrentUseCase(t)
	tor, m := moveFixture(t, uc, false, []byte("old library"))
	dir := uc.engine.DownloadDir()
	closeFirst()
	db, err := repository.NewSQLiteDB(filepath.Join(dir, "hub.db"))
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
	if err := restored.RestoreTorrents(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, payload.VolumeMarker)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("upgrade silently enrolled", err)
	}
	current, _ := restored.repo.GetTorrent(tor.Hash)
	if current.Storage.InventoryReason != "volume_unregistered" {
		t.Fatal(current.Storage)
	}
	cmd := models.StorageCommand{Action: "reconcile_storage", RecordID: tor.Control.RecordID, StorageID: m.StorageID, MappingRevision: m.Revision}
	if _, err := restored.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		current, _ = restored.repo.GetTorrent(tor.Hash)
		if current.Storage.Inventory == "ready" {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("reconcile did not inspect", current.Storage)
		case <-tick.C:
		}
	}
	if current.Control.DesiredRun != models.RunPaused {
		t.Fatal("reconcile resumed")
	}
}

func reopenInventoryFixture(t *testing.T, dir string) (*TorrentUseCase, func()) {
	t.Helper()
	db, err := repository.NewSQLiteDB(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(&config.Config{DBPath: filepath.Join(dir, "hub.db"), DownloadDir: dir, BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true, BTDisableUTP: true, BTRetrackersMode: "off", MaxActiveDownloads: 1})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	return NewTorrentUseCase(e, repository.NewTorrentRepo(db)), func() { e.Close(); db.Close() }
}

func TestInventoryExpectedMissingSurvivesRestart(t *testing.T) {
	uc, repo, closeFirst := setupTorrentUseCase(t)
	tor, m := moveFixture(t, uc, false, []byte("known data"))
	dir := uc.engine.DownloadDir()
	if err := repo.QueueInventory(m, tor.Control.RecordID, uc.inventorySession); err != nil {
		t.Fatal(err)
	}
	if ready, err := uc.inspectInventory(context.Background(), tor.Hash, false); err != nil || !ready {
		t.Fatal("initial inventory", ready, err)
	}
	closeFirst()
	if err := os.Remove(filepath.Join(dir, m.Root, "video.bin")); err != nil {
		t.Fatal(err)
	}
	restored, closeSecond := reopenInventoryFixture(t, dir)
	defer closeSecond()
	if err := restored.RestoreTorrents(); err != nil {
		t.Fatal(err)
	}
	current, err := restored.repo.GetTorrent(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if current.Storage.Inventory != "blocked" || current.Storage.InventoryReason != "expected_file_missing" {
		t.Fatalf("missing not diagnosed: %+v", current.Storage)
	}
	if restored.engine.GetRawTorrent(tor.Hash) != nil {
		t.Fatal("opened missing payload before repair")
	}
	if _, err := os.Stat(filepath.Join(dir, m.Root, "video.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("recreated missing file", err)
	}
	if err := restored.Resume(tor.Hash); err != nil {
		t.Fatal("explicit repair", err)
	}
}

func TestInventoryUncreatedAndSkipAreNotLost(t *testing.T) {
	uc, repo, done := setupTorrentUseCase(t)
	defer done()
	tor, m := moveFixture(t, uc, false, []byte("never inventoried"))
	if err := os.Remove(filepath.Join(uc.engine.DownloadDir(), m.Root, "video.bin")); err != nil {
		t.Fatal(err)
	}
	if err := repo.QueueInventory(m, tor.Control.RecordID, uc.inventorySession); err != nil {
		t.Fatal(err)
	}
	if ready, err := uc.inspectInventory(context.Background(), tor.Hash, false); err != nil || !ready {
		t.Fatal("uncreated file reported lost", ready, err)
	}
	files, err := repo.InventoryFiles(m, tor.Control.RecordID, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Expected {
		t.Fatal("invented expected presence", files)
	}
}

func TestInventoryVolumeMarkerBlocksWritesCleanupAndRecreation(t *testing.T) {
	uc, repo, done := setupTorrentUseCase(t)
	defer done()
	tor, m := moveFixture(t, uc, false, []byte("keep"))
	marker := filepath.Join(uc.engine.DownloadDir(), payload.VolumeMarker)
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := uc.guardPayloadIO(filepath.Join(m.Root, "new-file")); !errors.Is(err, payload.ErrVolumeUnavailable) {
		t.Fatal("write guard", err)
	}
	cmd := models.StorageCommand{Action: "reconcile_storage", RecordID: tor.Control.RecordID, StorageID: m.StorageID, MappingRevision: m.Revision}
	if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); !errors.Is(err, payload.ErrVolumeUnavailable) {
		t.Fatal("re-enrolled missing volume", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("marker recreated")
	}
	if err := uc.Delete(tor.Hash, true); err == nil {
		t.Fatal("cleanup ignored unavailable volume")
	}
	if b, err := os.ReadFile(filepath.Join(uc.engine.DownloadDir(), m.Root, "video.bin")); err != nil || string(b) != "keep" {
		t.Fatal("removed payload", err)
	}
	if err := os.WriteFile(marker, data, 0600); err != nil {
		t.Fatal(err)
	}
	// The failed deletion is its own workflow; restoring a marker alone must
	// not silently clear that journal or change paused intent.
	current, _ := repo.GetTorrent(tor.Hash)
	if current == nil || current.Control.DesiredRun != models.RunPaused {
		t.Fatal("volume restoration resumed payload")
	}
}
