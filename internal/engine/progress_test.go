package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/storage"
)

func TestVerifiedProgressRevokesReplacedPayload(t *testing.T) {
	e, tor := setupCompletedLifecycleTorrent(t)
	model := e.GetTorrent(tor.Hash)
	if model.ProgressSource != "verified" || model.Progress != 100 || model.Downloaded != model.Size || !model.MetadataKnown || !model.Files[0].OffsetKnown {
		t.Fatalf("verified model incorrect: %+v", model)
	}
	root, err := e.StorageRoot(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(e.DownloadDir(), root, "lifecycle", "video.mp4")
	if err := os.Remove(full); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, make([]byte, 2048), 0600); err != nil {
		t.Fatal(err)
	}
	model = e.GetTorrent(tor.Hash)
	if model.Downloaded != 0 || model.Progress != 0 || model.Files[0].Downloaded != 0 {
		t.Fatalf("replacement retained verified progress: %+v", model)
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
