package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBTHealthStorageIOTimings(t *testing.T) {
	e, model := setupCompletedLifecycleTorrent(t)
	raw := e.GetRawTorrent(model.Hash)
	before, ok := raw.StorageIOTimings()
	if !ok {
		t.Fatal("managed file backend has no timing diagnostics")
	}
	for i := 0; i < 3; i++ {
		h := e.BTHealth()
		if len(h.Torrents) != 1 || h.Torrents[0].StorageIO == nil {
			t.Fatal("storage timings missing", h)
		}
		io := h.Torrents[0].StorageIO
		if io.CompletionSyncCount != before.CompletionSyncCount || io.CompletionSyncNS != before.CompletionSyncNS || io.WriteLeaseWaitNS != before.WriteLeaseWaitNS {
			t.Fatal("incorrect timing projection", io, before)
		}
		wire, err := json.Marshal(h)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(wire), `"storage_io":{`) || !strings.Contains(string(wire), `"completion_sync_active":`) || !strings.Contains(string(wire), `"write_lease_wait_max_ns":`) {
			t.Fatal(string(wire))
		}
	}
	after, _ := raw.StorageIOTimings()
	if after != before {
		t.Fatal("health polling changed I/O counters", before, after)
	}
}
