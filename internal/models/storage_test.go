package models

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestStorageLocationContract(t *testing.T) {
	data, err := os.ReadFile("testdata/storage-location.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Storage   StorageLocation   `json:"storage"`
		File      FileStatus        `json:"file"`
		Selection SelectionProgress `json:"selection"`
		Empty     SelectionProgress `json:"empty_selection"`
		Command   StorageCommand    `json:"command"`
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	if v.Storage.Revision != 9007199254740993 || v.Command.MappingRevision != v.Storage.Revision {
		t.Fatal("revision lost precision")
	}
	if v.Empty.Progress != nil || v.Selection.Progress == nil || *v.Selection.Progress != 100 {
		t.Fatal("empty selection must not be complete")
	}
	if v.File.DirectReadReady || v.File.VerifiedBytes != 0 || v.File.LastKnownBytes != 16 {
		t.Fatal("historical bytes granted readiness")
	}
	encoded, err := json.Marshal(v)
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
		t.Fatalf("contract changed: %s", encoded)
	}
}
