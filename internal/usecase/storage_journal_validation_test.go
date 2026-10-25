package usecase

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"
)

func TestStorageJournalRejectsForeignPaths(t *testing.T) {
	for _, mutation := range []struct {
		name string
		edit func(*storageLocationJournal)
	}{
		{"staging", func(j *storageLocationJournal) { j.Staging = filepath.FromSlash("payload/another") }},
		{"cleanup", func(j *storageLocationJournal) { j.CleanupPath = filepath.FromSlash("payload/another") }},
		{"source", func(j *storageLocationJournal) { j.Entries[0].Source = "../another/file" }},
		{"target", func(j *storageLocationJournal) { j.Entries[0].Target = "../another/file" }},
		{"directory", func(j *storageLocationJournal) { j.Directories = []string{"../another"} }},
		{"hash", func(j *storageLocationJournal) { j.Source.Hash = "other" }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			v := &repository.RecoveryRecord{Hash: "h", RecordID: "record", Version: 1}
			j := &storageLocationJournal{Schema: 1, Source: &repository.StorageMapping{Hash: "h", StorageID: "storage", Revision: 1, Version: 2, Root: filepath.FromSlash("payload/source"), State: "ready"}, Target: payload.Layout{Version: 2, Root: filepath.FromSlash("payload/target")}, Staging: filepath.FromSlash(".tsh-recovery/moves/storage/record-1")}
			e := recoveryFingerprint{Identity: "file", Size: 8, Digest: strings.Repeat("0", 64)}
			j.SourceTree.Entries = []storageTreeEntry{{Path: "file", Evidence: e}}
			j.Entries = []storageMoveEntry{{Source: "file", Target: "file", Evidence: e}}
			if err := validateLocationJournal(v, j); err != nil {
				t.Fatal(err)
			}
			mutation.edit(j)
			data, _ := json.Marshal(j)
			v.Journal = string(data)
			if _, err := decodeLocationJournal(v); err == nil {
				t.Fatal("accepted foreign journal path")
			}
		})
	}
}
