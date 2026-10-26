package models

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestResumeWireFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/resume.json")
	if err != nil {
		t.Fatal(err)
	}
	var got ResumeProgress
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.CheckpointGeneration != "9007199254740993" || got.RestoredBytes != 16384 || got.FreshlyVerifiedBytes != 8192 {
		t.Fatalf("bad fixture: %+v", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	_ = json.Unmarshal(data, &a)
	_ = json.Unmarshal(encoded, &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("wire changed: %s", encoded)
	}
}
