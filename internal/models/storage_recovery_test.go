package models

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestStorageRecoveryJSONContract(t *testing.T) {
	data, err := os.ReadFile("testdata/storage-recovery.json")
	if err != nil {
		t.Fatal(err)
	}
	var statuses []StorageRecovery
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&statuses); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(statuses)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("wire contract changed: %s", encoded)
	}
	if statuses[0].Version != 9007199254740993 {
		t.Fatal("operation version lost precision")
	}
	reasons := []StorageRecoveryReason{StorageLegacyConflict, StorageDeletionPending, StorageDeletionCancelled, StorageDeletionMode, StorageCatalogChanged, StorageMigrationQueued, StorageMigrationActive, StorageAttachmentFailed, StorageInsufficientSpace}
	for i, reason := range reasons {
		if statuses[i].Reason != reason {
			t.Fatalf("reason %d: %q", i, statuses[i].Reason)
		}
	}
	if statuses[5].Progress.BytesTotal != nil || statuses[9].Progress.BytesTotal == nil || *statuses[9].Progress.BytesTotal != 0 {
		t.Fatal("unknown and empty totals must remain distinct")
	}
}

func TestStorageRecoveryIsAdditive(t *testing.T) {
	data, err := json.Marshal(Torrent{StorageState: "ready", Error: ErrMissingFiles})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["storage_recovery"]; exists {
		t.Fatal("fabricated recovery for legacy response")
	}
	if fields["error"] != "Missing files" || fields["storage_state"] != "ready" {
		t.Fatal("legacy fields changed")
	}
}

func TestStorageActionErrorContract(t *testing.T) {
	const data = `{"error":"deletion mode differs","code":"deletion_mode_conflict","storage_recovery":{"record_id":"record-a","version":"4","reason":"deletion_mode_conflict","actions":["retry_deletion","cancel_deletion"],"runtime":"absent","data_readiness":"unknown"}}`
	var status StorageActionError
	decoder := json.NewDecoder(bytes.NewBufferString(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&status); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != data {
		t.Fatalf("action error contract changed: %s", encoded)
	}
}
