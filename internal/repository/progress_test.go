package repository

import (
	"path/filepath"
	"testing"

	"torrent-stream-hub/internal/models"
)

func TestProgressObservationOrderingAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observations.db")
	db, err := NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	repo := NewTorrentRepo(db)
	tor := &models.Torrent{Hash: "observed", Name: "observed", Size: 8, Downloaded: 8, State: models.StatePaused,
		ProgressSource: "verified", ProgressObservedAt: 10, MetadataKnown: true, PieceLength: 4, NumPieces: 2,
		Files: []*models.File{{Index: 0, Path: "video", Size: 8, Downloaded: 8, Offset: 4, OffsetKnown: true, Priority: models.PriorityHigh}}}
	if err := repo.SaveTorrent(tor); err != nil {
		t.Fatal(err)
	}
	stale := *tor
	tor.Downloaded, tor.Files[0].Downloaded, tor.ProgressObservedAt = 0, 0, 20
	if err := repo.SaveTorrent(tor); err != nil {
		t.Fatal(err)
	}
	stale.Files = []*models.File{{Index: 0, Path: "video", Size: 8, Downloaded: 8}}
	if err := repo.SaveTorrent(&stale); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveTorrent(&models.Torrent{Hash: tor.Hash, ProgressSource: "unknown", State: models.StatePaused}); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{"observed"} {
		saved, err := repo.GetTorrent(hash)
		if err != nil {
			t.Fatal(err)
		}
		if saved.Downloaded != 0 || saved.Files[0].Downloaded != 0 || saved.Progress != 0 || saved.ProgressSource != "last_known" || saved.ProgressObservedAt != 20 {
			t.Fatalf("stale snapshot restored bytes: %+v", saved)
		}
		if !saved.MetadataKnown || saved.PieceLength != 4 || saved.NumPieces != 2 || !saved.Files[0].OffsetKnown || saved.Files[0].Offset != 4 {
			t.Fatal("known piece metadata lost")
		}
		if saved.Size != 8 || saved.Name != "observed" {
			t.Fatal("metadata-pending snapshot erased known metadata")
		}
		// A metadata/command save from an older DB-only view must not clobber a
		// newer runtime observation or overwrite authoritative priority intent.
		saved.Downloaded, saved.Files[0].Downloaded = 8, 8
		if err := repo.SaveTorrent(saved); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	db, err = NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	repo = NewTorrentRepo(db)
	all, err := repo.GetAllTorrents()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Downloaded != 0 || all[0].Files[0].Downloaded != 0 || all[0].Files[0].Priority != models.PriorityHigh || all[0].ProgressSource != "last_known" {
		t.Fatal("reopen changed observation", all)
	}
}

func TestProgressMigrationPreservesLegacyWithoutTrust(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	repo := NewTorrentRepo(db)
	if err := repo.SaveTorrent(&models.Torrent{Hash: "legacy", Size: 100, Downloaded: 60, State: models.StatePaused, Files: []*models.File{{Index: 0, Path: "video", Size: 100, Downloaded: 60}}}); err != nil {
		t.Fatal(err)
	}
	// Later-version triggers cannot exist in a pre-upgrade fixture.
	if _, err := db.DB().Exec(`DROP TRIGGER version_torrents_update; DROP TRIGGER version_files_update`); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the pre-0008 schema and execute the real upgrade on reopen.
	for _, column := range []string{"legacy_downloaded", "progress_observed_at", "progress_source", "piece_length", "num_pieces", "metadata_known"} {
		if _, err := db.DB().Exec(`ALTER TABLE torrents DROP COLUMN ` + column); err != nil {
			t.Fatal(err)
		}
	}
	for _, column := range []string{"legacy_downloaded", "file_offset", "offset_known"} {
		if _, err := db.DB().Exec(`ALTER TABLE files DROP COLUMN ` + column); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB().Exec(`DELETE FROM migrations WHERE name='0008_progress_observations.sql'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := NewTorrentRepo(db).GetTorrent("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Progress != 60 || saved.ProgressSource != "legacy" || saved.LegacyDownloaded != 60 || saved.MetadataKnown || saved.Files[0].OffsetKnown || saved.Files[0].LegacyDownloaded != 60 {
		t.Fatalf("legacy misrepresented as verified: %+v", saved)
	}
}

func TestProgressPercentBoundaries(t *testing.T) {
	for _, tc := range []struct {
		bytes, size int64
		want        float64
	}{{0, 0, 0}, {5, 0, 0}, {-1, 10, 0}, {11, 10, 100}, {5, 10, 50}} {
		if got := models.ProgressPercent(tc.bytes, tc.size); got != tc.want {
			t.Fatalf("%+v: %v", tc, got)
		}
	}
}
