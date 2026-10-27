package torrent

import (
	"sync/atomic"
	"testing"
)

func TestStorageIOTimingsCachedAndClosed(t *testing.T) {
	dir, mi, _, checkpoint := warmCheckpoint(t, "v1")
	var events atomic.Int64
	cl, _ := checkpointClient(t, dir, &checkpoint, func(string, int64) { events.Add(1) })
	tor := addCheckpointTorrent(t, cl, mi, nil)
	waitVerificationIdle(t, tor)
	before := events.Load()
	for i := 0; i < 10; i++ {
		if _, ok := tor.StorageIOTimings(); !ok {
			t.Fatal("missing cached diagnostics")
		}
	}
	if events.Load() != before {
		t.Fatal("timing polling performed storage I/O")
	}
	tor.Drop()
	if _, ok := tor.StorageIOTimings(); ok {
		t.Fatal("closed attachment diagnostics exposed")
	}
}
