package repository

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
)

func TestInventoryPublicationFencesMappingAndReadd(t *testing.T) {
	db, err := NewSQLiteDB(filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewTorrentRepo(db)
	const hash = "record"
	if err := r.SaveTorrent(&models.Torrent{Hash: hash, Files: []*models.File{{Index: 0, Path: "file", Size: 8}}}); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveStorageMapping(&StorageMapping{Hash: hash, Version: 2, Root: filepath.Join("payload", "one"), State: "ready"}); err != nil {
		t.Fatal(err)
	}
	tor, _ := r.GetTorrent(hash)
	m, _ := r.StorageMapping(hash)
	if err := r.BeginInventorySession("session"); err != nil {
		t.Fatal(err)
	}
	files := []InventoryFile{{Index: 0, Path: "file", Size: 8, Expected: true, Presence: "present", ObservedAt: 1}}
	if err := r.SaveInventory(m, tor.Control.RecordID, "session", "ready", "", 0, files, false); err != nil {
		t.Fatal(err)
	}
	if err := r.BeginInventorySession("reopened"); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveInventory(m, tor.Control.RecordID, "session", "ready", "", 0, files, false); !errors.Is(err, ErrStateConflict) {
		t.Fatal("old process published observation", err)
	}
	if err := r.PrimeNewInventory(m, "session"); !errors.Is(err, ErrStateConflict) {
		t.Fatal("old process admitted payload", err)
	}
	if err := r.SetStorageLayout(context.Background(), m, tor.Control.RecordID, payload.Layout{Version: 2, Root: filepath.Join("payload", "two")}); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveInventory(m, tor.Control.RecordID, "old-session", "ready", "", 0, files, false); !errors.Is(err, ErrStateConflict) {
		t.Fatal("old mapping observation", err)
	}
	next, _ := r.StorageMapping(hash)
	observed, err := r.InventoryFiles(next, tor.Control.RecordID, -1)
	if err != nil || len(observed) != 1 || !observed[0].Expected {
		t.Fatal("move lost expected presence", observed, err)
	}
	state, _ := r.Inventory(m.StorageID)
	if state.Status != "pending" {
		t.Fatal("move adopted old freshness")
	}
	if err := r.DeleteTorrent(hash); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveTorrent(&models.Torrent{Hash: hash, Files: []*models.File{{Index: 0, Path: "file", Size: 8}}}); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveInventory(next, tor.Control.RecordID, "session", "ready", "", 0, files, false); !errors.Is(err, ErrStateConflict) {
		t.Fatal("deleted record published inventory", err)
	}
}
