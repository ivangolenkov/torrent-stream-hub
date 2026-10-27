package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"torrent-stream-hub/internal/models"

	"github.com/anacrolix/torrent/storage"
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
		gotStages := []models.StorageIOStageTimings{io.WriteLeaseHold, io.WritePrepare, io.WriteGuard, io.WriteOpen, io.WriteStat, io.WritePayload, io.WriteAccount, io.WriteClose, io.WriteOpenGuard, io.WriteOpenPath, io.WriteOpenMkdir, io.WriteOpenDescriptor, io.WriteOpenIdentity}
		stages := []storage.IOStageTimings{before.WriteLeaseHold, before.WritePrepare, before.WriteGuard, before.WriteOpen, before.WriteStat, before.WritePayload, before.WriteAccount, before.WriteClose, before.WriteOpenGuard, before.WriteOpenPath, before.WriteOpenMkdir, before.WriteOpenDescriptor, before.WriteOpenIdentity}
		for i, stage := range stages {
			if gotStages[i] != storageIOStage(stage) {
				t.Fatalf("stage %d projection differs", i)
			}
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

func TestStorageIOStageProjection(t *testing.T) {
	s := storage.IOStageTimings{Count: 7, Errors: 2, Active: 1, NS: 91, MaxNS: 43}
	got := storageIOStage(s)
	if got.Count != 7 || got.Errors != 2 || got.Active != 1 || got.NS != 91 || got.MaxNS != 43 {
		t.Fatal(got)
	}
}
