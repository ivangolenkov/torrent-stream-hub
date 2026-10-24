package repository

import (
	"math"
	"testing"
	"torrent-stream-hub/internal/models"
)

func TestPhase40CatalogLayouts(t *testing.T) {
	for _, format := range []string{"v1", "v2", "hybrid"} {
		t.Run(format, func(t *testing.T) {
			c := models.TorrentCatalog{Format: format, Size: 2048, PieceLength: 16384, NumPieces: 1, Files: []models.FileCatalog{{Path: "a", Size: 1024}, {Index: 1, Path: "empty", Offset: 1024}, {Index: 2, Path: "b", Size: 1024, Offset: 1024}}}
			if format != "v1" {
				c.NumPieces = 2
				c.Files[1].Offset = 16384
				c.Files[2].Offset = 16384
			}
			if err := validateCatalog(c); err != nil {
				t.Fatal(err)
			}
			c.Files[2].Offset++
			if err := validateCatalog(c); err == nil {
				t.Fatal("accepted invalid offset")
			}
		})
	}
	c := models.TorrentCatalog{Format: "v2", Size: math.MaxInt64, PieceLength: 16384, NumPieces: 1, Files: []models.FileCatalog{{Path: "a", Size: math.MaxInt64}}}
	if err := validateCatalog(c); err == nil {
		t.Fatal("accepted overflow")
	}
}

func TestPhase40CompositeMetadataRollbackAndNoop(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	r := NewTorrentRepo(db)
	i := intentFixture(t, r, "composite")
	if _, err := db.DB().Exec(`CREATE TRIGGER fail_permanence BEFORE UPDATE OF is_temporary ON torrents BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CommitAddMetadata(i.Version, models.UserMetadata{Title: "changed"}, true); err == nil {
		t.Fatal("ignored transaction failure")
	}
	got, err := r.GetTorrent(i.Version.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "user title" || !got.IsTemporary || got.Control.DesiredRevision != i.Version.Revision {
		t.Fatal("partial command committed", got)
	}
	if _, err := db.DB().Exec(`DROP TRIGGER fail_permanence`); err != nil {
		t.Fatal(err)
	}
	v, err := r.CommitAddMetadata(i.Version, models.UserMetadata{Title: "changed"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if v.Revision != i.Version.Revision+1 {
		t.Fatal(v)
	}
	again, err := r.CommitAddMetadata(v, models.UserMetadata{Title: "changed"}, true)
	if err != nil || again != v {
		t.Fatal(again, err)
	}
}
