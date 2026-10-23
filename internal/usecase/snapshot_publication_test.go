package usecase

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestSnapshotPublicationPairsCaptureVersionAcrossDelayedJSON(t *testing.T) {
	uc, _, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor := temporaryFile(t, uc, "snapshot-publication", false)
	sw := NewSyncWorker(uc)
	defer sw.Close()
	ch := make(chan PublishedSnapshot, 1)
	sw.AddSnapshotClient(ch)
	defer sw.RemoveSnapshotClient(ch)
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	sw.marshal = func(v any) ([]byte, error) { close(entered); <-release; return json.Marshal(v) }
	go func() { sw.Refresh(); close(finished) }()
	<-entered
	if err := uc.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	receipt := strings.Split(uc.NextSnapshotVersion(), "/")
	unblock()
	<-finished
	publication := <-ch
	version := strings.Split(publication.Version, "/")
	a, err := strconv.ParseUint(version[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	b, err := strconv.ParseUint(receipt[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if version[0] != receipt[0] || a >= b {
		t.Fatal("encoding relabelled a pre-command DTO", publication.Version, receipt)
	}
	latest := make(chan PublishedSnapshot, 1)
	sw.AddSnapshotClient(latest)
	defer sw.RemoveSnapshotClient(latest)
	if next := <-latest; next.Version != publication.Version || string(next.Data) != string(publication.Data) {
		t.Fatal("version and data came from different publications")
	}
}

func TestEmptySnapshotHasVersionAndLatestOnlyPair(t *testing.T) {
	sw := NewSyncWorker(nil)
	defer sw.Close()
	ch := make(chan PublishedSnapshot, 1)
	sw.AddSnapshotClient(ch)
	sw.publishSnapshot([]byte(`[1]`), "epoch/1")
	sw.publishSnapshot([]byte(`[]`), "epoch/2")
	if got := <-ch; got.Version != "epoch/2" || string(got.Data) != "[]" {
		t.Fatal(got)
	}
	sw.RemoveSnapshotClient(ch)
	sw.RemoveSnapshotClient(ch)
}
