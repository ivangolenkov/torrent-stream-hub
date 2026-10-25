package usecase

import (
	"path/filepath"
	"testing"
	"torrent-stream-hub/internal/models"
)

func fileProjectionFixture() (*models.Torrent, *models.Torrent) {
	saved := &models.Torrent{Hash: "one", Size: 8, Downloaded: 8, Progress: 100, ProgressSource: "last_known", ProgressObservedAt: 5e9, MetadataKnown: true, CatalogVersion: "catalog",
		Control: &models.TorrentControl{RecordID: "record", DesiredRevision: 3},
		Storage: &models.StorageLocation{ID: "storage", Revision: 2, LayoutVersion: 2, Root: filepath.FromSlash("payload/Readable"), Prefix: "Original", Status: "ready", Inventory: "ready"},
		Files:   []*models.File{{Index: 0, Path: "a", MetadataPath: filepath.FromSlash("Original/a"), Size: 4, Downloaded: 4, Status: &models.FileStatus{Presence: "present", ObservedAtMS: 4000}}, {Index: 1, Path: "b", MetadataPath: filepath.FromSlash("Original/b"), Size: 4, Downloaded: 4, Priority: models.PriorityNone, Status: &models.FileStatus{Presence: "present"}}}}
	live := &models.Torrent{Hash: "one", Size: 8, Downloaded: 4, Progress: 50, ProgressSource: "verified", MetadataKnown: true,
		Control: &models.TorrentControl{RecordID: "record", RuntimeAttachment: "open", AppliedRevision: 3},
		Files:   []*models.File{{Index: 0, Path: "a", Size: 4, Downloaded: 4, Status: &models.FileStatus{VerifiedBytes: 4, CheckedBytes: 4, DirectReadReady: true, Verification: "completed", EffectivePriority: "high"}}, {Index: 1, Path: "b", Size: 4, Status: &models.FileStatus{Verification: "completed", EffectivePriority: "none"}}}}
	return live, saved
}

func TestFileProjectionSelectionAndHistoricalProgress(t *testing.T) {
	live, saved := fileProjectionFixture()
	got := projectTorrent(live, saved)
	if got.Progress != 50 || got.Downloaded != 4 || got.Selection == nil || got.Selection.Progress == nil || *got.Selection.Progress != 100 || !got.Selection.Complete || got.Selection.Size != 4 || got.Selection.Revision != 3 {
		t.Fatalf("selection/whole mixed: %+v", got.Selection)
	}
	if got.LastKnownBytes != 8 || got.Files[0].Status.LastKnownBytes != 4 || got.Files[0].Status.LastKnownAtMS != 5000 || !got.Files[0].Status.DirectReadReady {
		t.Fatal("lost provenance", got.Files[0].Status)
	}
	if got.Files[0].Status.StoragePath != filepath.FromSlash("payload/Readable/a") {
		t.Fatal("wrong physical path", got.Files[0].Status)
	}
	if got.Files[1].Priority != models.PriorityNone {
		t.Fatal("runtime replaced base selection")
	}
	compact := CompactTorrents([]*models.Torrent{got})[0]
	if compact.Files != nil || compact.Selection != got.Selection {
		t.Fatal("compact lost summary or repeated catalog")
	}
	live, saved = fileProjectionFixture()
	live.ProgressSource, live.Downloaded, live.Progress = "checking", 0, 0
	live.Files[0].Status.DirectReadReady = false
	got = projectTorrent(live, saved)
	if got.LastKnownBytes != 8 || got.Files[0].Status.VerifiedBytes != 4 || got.Files[0].Status.DirectReadReady {
		t.Fatal("checking erased history or granted read")
	}
}

func TestFileProjectionEmptySkippedAndUnavailable(t *testing.T) {
	for _, scenario := range []string{"empty-selection", "skip-ready", "zero-unknown", "zero-present", "volume", "pending", "detached", "disk-full"} {
		t.Run(scenario, func(t *testing.T) {
			live, saved := fileProjectionFixture()
			switch scenario {
			case "empty-selection", "skip-ready":
				saved.Files[0].Priority = models.PriorityNone
			case "zero-unknown", "zero-present":
				live.Files[0].Size, live.Files[0].Status.VerifiedBytes, saved.Files[0].Size = 0, 0, 0
				live.Files[0].Status.DirectReadReady = false
				if scenario == "zero-unknown" {
					saved.Files[0].Status.Presence = "unknown"
				}
			case "volume":
				saved.Storage.Inventory, saved.Storage.InventoryReason = "blocked", "volume_unavailable"
			case "pending":
				saved.Storage.Inventory = "pending"
			case "detached":
				live = nil
			case "disk-full":
				live.DownloadBlock = "Disk full"
			}
			got := projectTorrent(live, saved)
			s := got.Files[0].Status
			switch scenario {
			case "empty-selection", "skip-ready":
				if got.Selection.Progress != nil || got.Selection.Complete || got.Selection.Files != 0 || !s.DirectReadReady {
					t.Fatal(got.Selection, s)
				}
			case "zero-unknown":
				if got.Selection.Progress != nil || got.Selection.Complete || s.DirectReadReady {
					t.Fatal(got.Selection, s)
				}
			case "zero-present":
				if !got.Selection.Complete || !s.DirectReadReady || got.Selection.Progress != nil {
					t.Fatal(got.Selection, s)
				}
			case "volume":
				if s.DirectReadReady || s.Presence != "unavailable" || s.ReadReason != "volume_unavailable" {
					t.Fatal(s)
				}
			case "pending":
				if s.DirectReadReady || s.Presence != "unknown" {
					t.Fatal(s)
				}
			case "detached":
				if s.VerifiedBytes != 0 || s.DirectReadReady || s.LastKnownBytes != 4 || got.Selection.Complete {
					t.Fatal(s)
				}
			case "disk-full":
				if !s.DirectReadReady {
					t.Fatal("disk write block denied verified local reading")
				}
			}
		})
	}
}
