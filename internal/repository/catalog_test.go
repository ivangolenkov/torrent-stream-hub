package repository

import (
	"errors"
	"testing"

	"torrent-stream-hub/internal/models"
)

func TestCatalogPreservesSelectionAndDoesNotWriteObservation(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	r := NewTorrentRepo(db)
	i := intentFixture(t, r, "catalog")
	f, err := r.AttachRuntime(i.Version)
	if err != nil {
		t.Fatal(err)
	}
	c := models.TorrentCatalog{Name: "catalog", Size: 8, PieceLength: 4, NumPieces: 2, Files: []models.FileCatalog{{Index: 0, Path: "video", Size: 8}}}
	if err := r.SaveCatalog(f, c); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveRuntimeObservation(verifiedObservation(f, 1, 8, 10)); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateFilePriority(f.Hash, 0, models.PriorityNone); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveCatalog(f, c); !errors.Is(err, ErrStateConflict) {
		t.Fatal("stale revision wrote catalog", err)
	}
	intent, err := r.GetIntent(f.Hash)
	if err != nil {
		t.Fatal(err)
	}
	f, err = r.AttachRuntime(intent.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SaveCatalog(f, c); err != nil {
		t.Fatal(err)
	}
	saved, err := r.GetTorrent(f.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Downloaded != 8 || saved.Title != "user title" || saved.Files[0].Priority != models.PriorityNone || !saved.Files[0].OffsetKnown {
		t.Fatal(saved)
	}
	c.Files[0].Path = "other"
	if err := r.SaveCatalog(f, c); !errors.Is(err, ErrStateConflict) {
		t.Fatal("catalog replaced known file identity", err)
	}
}

func TestCatalogDiscoveryDefaultsAndRollback(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	r := NewTorrentRepo(db)
	if err := r.SaveTorrent(&models.Torrent{Hash: "pending", State: models.StatePaused}); err != nil {
		t.Fatal(err)
	}
	i, err := r.GetIntent("pending")
	if err != nil {
		t.Fatal(err)
	}
	f, err := r.AttachRuntime(i.Version)
	if err != nil {
		t.Fatal(err)
	}
	c := models.TorrentCatalog{Name: "discovered", Size: 8, PieceLength: 4, NumPieces: 2, Files: []models.FileCatalog{{Index: 0, Path: "a", Size: 4}, {Index: 1, Path: "b", Size: 4, Offset: 4}}}
	if _, err := db.DB().Exec(`CREATE TRIGGER reject_catalog_file BEFORE INSERT ON files WHEN NEW."index"=1 BEGIN SELECT RAISE(ABORT,'injected catalog failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveCatalog(f, c); err == nil {
		t.Fatal("ignored catalog failure")
	}
	saved, err := r.GetTorrent(f.Hash)
	if err != nil || saved.Size != 0 || saved.MetadataKnown || len(saved.Files) != 0 {
		t.Fatal("partial catalog survived rollback", saved, err)
	}
	if _, err := db.DB().Exec(`DROP TRIGGER reject_catalog_file`); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveCatalog(f, c); err != nil {
		t.Fatal(err)
	}
	saved, err = r.GetTorrent(f.Hash)
	if err != nil || len(saved.Files) != 2 || saved.Files[1].Priority != models.PriorityNormal || saved.Files[1].Downloaded != 0 {
		t.Fatal(saved, err)
	}
	if err := r.DeleteTorrent(f.Hash); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveCatalog(f, c); !errors.Is(err, ErrStateConflict) {
		t.Fatal("resurrected catalog", err)
	}
}

func TestIntentMetadataFailureRollsBackRevision(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	r := NewTorrentRepo(db)
	i := intentFixture(t, r, "metadata-failure")
	if _, err := db.DB().Exec(`CREATE TRIGGER reject_metadata BEFORE UPDATE OF title ON torrents BEGIN SELECT RAISE(ABORT,'injected metadata failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.UpdateUserMetadata(i.Version, models.UserMetadata{Title: "new"}); err == nil {
		t.Fatal("ignored metadata failure")
	}
	after, err := r.GetIntent(i.Version.Hash)
	if err != nil || *after != *i {
		t.Fatal(after, err)
	}
	saved, err := r.GetTorrent(i.Version.Hash)
	if err != nil || saved.Title != "user title" {
		t.Fatal(saved, err)
	}
}

func TestCreateTorrentIsAtomicInsertOnlyAndHasNoVerifiedBytes(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	r := NewTorrentRepo(db)
	c := models.TorrentCatalog{Name: "created", Size: 4, PieceLength: 4, NumPieces: 1, Files: []models.FileCatalog{{Index: 0, Path: "video", Size: 4}}}
	n := models.NewTorrentRecord{Hash: "created", Name: "created", Run: models.RunEnabled, Temporary: true, Catalog: &c}
	if _, err := db.DB().Exec(`CREATE TRIGGER reject_create_file BEFORE INSERT ON files BEGIN SELECT RAISE(ABORT,'injected creation failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateTorrent(n); err == nil {
		t.Fatal("ignored creation failure")
	}
	if i, err := r.GetIntent(n.Hash); err != nil || i != nil {
		t.Fatal("partial intent survived", i, err)
	}
	if record, err := r.GetTorrent(n.Hash); err != nil || record != nil {
		t.Fatal("partial parent survived", record, err)
	}
	if _, err := db.DB().Exec(`DROP TRIGGER reject_create_file`); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateTorrent(n); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateTorrent(n); err == nil {
		t.Fatal("create overwrote existing record")
	}
	i, err := r.GetIntent(n.Hash)
	if err != nil || i.Run != models.RunEnabled {
		t.Fatal(i, err)
	}
	saved, err := r.GetTorrent(n.Hash)
	if err != nil || saved.Downloaded != 0 || saved.ProgressSource != "unknown" || saved.Files[0].Downloaded != 0 || !saved.MetadataKnown {
		t.Fatal(saved, err)
	}
}
