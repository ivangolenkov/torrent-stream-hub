package engine

import (
	"context"
	"testing"

	"torrent-stream-hub/internal/models"
)

func TestActivityIsOrthogonalAndGenerationScoped(t *testing.T) {
	e, mt := gateFixture(t)
	sm := e.streamManager
	stream, err := sm.addSession(mt.hash, 0, StreamOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer sm.removeSession(mt.hash, 0, stream, false)
	preload, err := sm.addSession(mt.hash, 0, StreamOptions{}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer sm.removeSession(mt.hash, 0, preload, true)
	view := e.GetTorrent(mt.hash)
	if view.State != models.StateDownloading || !view.Activity.Streaming || view.Activity.Streams != 1 || view.Activity.Preloads != 1 || view.Activity.Verifying {
		t.Fatal(view)
	}
	if got := sm.activityForTorrent(&ManagedTorrent{hash: mt.hash}); got != (models.TorrentActivity{}) {
		t.Fatal("activity leaked across same-hash identity", got)
	}
	if err := e.Pause(mt.hash); err != nil {
		t.Fatal(err)
	}
	if got := e.GetTorrent(mt.hash); got.State != models.StatePaused || !got.Activity.Streaming {
		t.Fatal("stream accounting replaced paused mode", got)
	}
}

func TestDirectPausedActivityAndHEADAccounting(t *testing.T) {
	e, tor := setupCompletedLifecycleTorrent(t)
	if err := e.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	head, err := e.OpenStreamSource(context.Background(), tor.Hash, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer head.Close()
	if got := e.GetTorrent(tor.Hash); got.Activity.Streaming || got.Activity.Streams != 0 || got.Activity.DirectReaders != 0 {
		t.Fatal("HEAD created activity", got.Activity)
	}
	src, err := e.OpenStreamSource(context.Background(), tor.Hash, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.GetTorrent(tor.Hash); got.State != models.StatePaused || !got.Activity.Streaming || got.Activity.DirectReaders != 1 || got.Activity.Streams != 0 {
		t.Fatal("disk reader lost paused intent", got)
	}
	src.Close()
	if got := e.GetTorrent(tor.Hash); got.Activity.Streaming {
		t.Fatal("closed reader retained activity", got.Activity)
	}
}
