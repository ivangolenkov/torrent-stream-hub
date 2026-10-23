package engine

import (
	"errors"
	"testing"

	"torrent-stream-hub/internal/models"
)

func TestSafetyFaultBeforeBindingPersistsAtObservedBind(t *testing.T) {
	e, mt := gateFixture(t)
	writes := 0
	want := models.RuntimeFence{IntentVersion: models.IntentVersion{Hash: mt.hash, RecordID: "record", Revision: 2}, Attachment: "attachment"}
	e.SetSafetyPersistence(func(f models.RuntimeFence, kind models.ActivationBlockKind, reason string) error {
		if f != want || kind != models.BlockMetadataPersistence || reason != string(models.ErrMetadataPersistence) {
			t.Fatal(f, kind, reason)
		}
		writes++
		return nil
	})
	e.admissionMu.Lock()
	err := e.quiesceFaultLocked(mt, stateMetadataFailed)
	e.admissionMu.Unlock()
	if err != nil || writes != 0 {
		t.Fatal(err, writes)
	}
	if err := e.BindRuntime(mt.t, want); !errors.Is(err, ErrOperationConflict) {
		t.Fatal("fault acknowledged", err)
	}
	if err := e.BindObservedRuntime(mt.t, want); err != nil || writes != 1 {
		t.Fatal(err, writes)
	}
}

func TestSafetyPersistenceFailureLeavesRuntimeQuiescent(t *testing.T) {
	e, mt := gateFixture(t)
	f := models.RuntimeFence{IntentVersion: models.IntentVersion{Hash: mt.hash, RecordID: "record", Revision: 1}, Attachment: "attachment"}
	if err := e.BindRuntime(mt.t, f); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("SQLite unavailable")
	e.SetSafetyPersistence(func(models.RuntimeFence, models.ActivationBlockKind, string) error { return injected })
	e.admissionMu.Lock()
	err := e.quiesceFaultLocked(mt, stateMetadataFailed)
	e.admissionMu.Unlock()
	if !errors.Is(err, injected) || mt.t.DataDownloadAllowed() || e.GetTorrent(mt.hash).Control.AppliedRevision != 0 {
		t.Fatal("persistence error admitted work", err)
	}
	f.Attachment = "replacement"
	if err := e.BindObservedRuntime(mt.t, f); !errors.Is(err, injected) {
		t.Fatal("bind ignored unpersisted safety", err)
	}
}
