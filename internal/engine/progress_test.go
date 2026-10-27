package engine

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/models"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

func TestAbsentStartupIsNotActiveOrVerifiedProgress(t *testing.T) {
	dir := t.TempDir()
	e, err := New(&config.Config{DownloadDir: dir, DBPath: filepath.Join(dir, "hub.db"), TorrentPort: 0, BTDisableDHT: true, BTDisablePEX: true, BTDisableTCP: true, BTDisableUTP: true, BTNoUpload: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	raw := createTestTorrentBytes(4, "new", []metainfo.FileInfo{{Length: 8, Path: []string{"data"}}}, [][]byte{[]byte("gooddata")})
	tor, err := e.AddTorrentFile(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	model := e.GetTorrent(tor.Hash)
	if model.Verification == nil || model.Verification.SkippedMissingBytes != 8 || model.Verification.CheckedBytes != 0 || model.Activity.Verifying || model.Downloaded != 0 || model.DirectReadReady {
		t.Fatalf("missing startup misrepresented: %+v", model)
	}
	if model.Files[0].Status.SkippedMissingBytes != 8 || model.Files[0].Status.ReadReason != "incomplete" {
		t.Fatal(model.Files[0].Status)
	}
	if e.GetTorrentCached(tor.Hash).Verification.ProgressAtMS != 0 {
		t.Fatal("negative evidence advanced hash progress timestamp")
	}
}

func TestManagedProgressRequiresRecheckAfterExternalChange(t *testing.T) {
	e, tor := setupCompletedLifecycleTorrent(t)
	model := e.GetTorrent(tor.Hash)
	if model.ProgressSource != "verified" || model.Progress != 100 || model.Downloaded != model.Size || !model.MetadataKnown || !model.Files[0].OffsetKnown {
		t.Fatalf("verified model incorrect: %+v", model)
	}
	if model.Files[0].Status == nil || !model.Files[0].Status.DirectReadReady || model.Files[0].Status.VerifiedBytes != model.Size || model.Files[0].Status.CheckedBytes != model.Size || model.Verification.Status != "completed" {
		t.Fatal("current-open observations missing", model.Files[0].Status, model.Verification)
	}
	if next := e.GetTorrentCached(tor.Hash); next.Verification.ProgressAtMS != model.Verification.ProgressAtMS {
		t.Fatal("polling invented verification progress")
	}
	root, err := e.StorageRoot(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(e.DownloadDir(), root, "lifecycle", "video.mp4")
	// In-place external changes are unsupported while attached. Polling no
	// longer performs a filesystem scan; explicit Recheck must detect corruption.
	if err := os.WriteFile(full, make([]byte, 2048), 0600); err != nil {
		t.Fatal(err)
	}
	model = e.GetTorrent(tor.Hash)
	if model.Downloaded != model.Size {
		t.Fatal("polling unexpectedly revalidated external payload")
	}
	if err := e.GetRawTorrent(tor.Hash).VerifyData(); err != nil {
		t.Fatal(err)
	}
	model = e.GetTorrent(tor.Hash)
	if model.Downloaded != 0 || model.Progress != 0 || model.Files[0].Downloaded != 0 {
		t.Fatalf("replacement retained verified progress: %+v", model)
	}
}

func TestCompletedEmptySelectionDoesNotClaimSeeding(t *testing.T) {
	e, tor := setupCompletedLifecycleTorrent(t)
	if err := e.Resume(tor.Hash); err != nil {
		t.Fatal(err)
	}
	if err := e.SetTorrentFilesPriority(tor.Hash, models.PriorityNone); err != nil {
		t.Fatal(err)
	}
	model := e.GetTorrent(tor.Hash)
	if model.State == models.StateSeeding || model.State == models.StateDownloading || !model.Files[0].Status.DirectReadReady {
		t.Fatal("empty selection claimed download/seeding or lost checked local bytes", model)
	}
	if err := e.SetFilePriority(tor.Hash, 0, models.PriorityNormal); err != nil {
		t.Fatal(err)
	}
	if model := e.GetTorrent(tor.Hash); model.State != models.StateSeeding {
		t.Fatal("selected complete payload did not return to seeding", model)
	}
}

func TestVerifiedPrefixOnlyCountsCompleteCheckedRuns(t *testing.T) {
	runs := torrent.PieceStateRuns{
		{Length: 1, PieceState: torrent.PieceState{Completion: storage.Completion{Ok: true, Complete: true}}},
		{Length: 1},
		{Length: 2, PieceState: torrent.PieceState{Completion: storage.Completion{Ok: true, Complete: true}}},
		{Length: 1, PieceState: torrent.PieceState{Completion: storage.Completion{Ok: true, Complete: true}, Checking: true}},
	}
	prefix := verifiedPrefix(runs, 4)
	for _, tc := range []struct{ offset, want int64 }{{0, 0}, {2, 2}, {4, 4}, {6, 4}, {8, 4}, {10, 6}, {16, 12}, {20, 12}} {
		if got := prefix(tc.offset); got != tc.want {
			t.Fatalf("offset %d got %d want %d", tc.offset, got, tc.want)
		}
	}
	if prefix(10)-prefix(2) != 4 {
		t.Fatal("shared boundary counted outside file")
	}
}
