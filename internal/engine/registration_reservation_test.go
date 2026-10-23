package engine

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

func TestRegistrationPersistenceDoesNotHoldAdmissionAndReservesIdentity(t *testing.T) {
	e, b := gateFixture(t)
	data := createTestTorrentBytes(4, "registration-reservation", []metainfo.FileInfo{{Length: 4, Path: []string{"file"}}}, [][]byte{[]byte("data")})
	mi, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	hash := mi.HashInfoBytes().HexString()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	e.admissionMu.Lock()
	e.registerMetainfo = func(hash string, mi *metainfo.MetaInfo) error {
		close(entered)
		<-release
		return e.saveMetainfo(hash, mi, true)
	}
	e.admissionMu.Unlock()
	added := make(chan error, 1)
	go func() { _, err := e.AddStagedTorrentFile(bytes.NewReader(data)); added <- err }()
	await(t, entered)
	paused := make(chan error, 1)
	go func() { paused <- e.Pause(b.hash) }()
	select {
	case err := <-paused:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		unblock()
		t.Fatal("B waited on A registration persistence")
	}
	if e.GetTorrent(hash) != nil {
		t.Fatal("unprepared registration published")
	}
	if err := e.Delete(hash); !errors.Is(err, ErrOperationConflict) {
		t.Fatal("Delete ignored registration reservation", err)
	}
	if _, err := e.AddStagedTorrentFile(bytes.NewReader(data)); !errors.Is(err, ErrOperationConflict) {
		t.Fatal("duplicate registration took same namespace", err)
	}
	unblock()
	if err := <-added; err != nil {
		t.Fatal(err)
	}
	e.admissionMu.Lock()
	pending := len(e.registering)
	e.registerMetainfo = nil
	e.admissionMu.Unlock()
	if pending != 0 || e.GetTorrent(hash) == nil {
		t.Fatal("registration reservation failed to retire", pending)
	}
	if e.GetRawTorrent(hash).DataDownloadAllowed() {
		t.Fatal("staged publication admitted payload before commit/full apply")
	}
}

func TestRegistrationFailureRetiresReservationsAndDoesNotPublish(t *testing.T) {
	e, _ := gateFixture(t)
	injected := errors.New("cached metainfo unavailable")
	e.admissionMu.Lock()
	e.registerMetainfo = func(string, *metainfo.MetaInfo) error { return injected }
	e.admissionMu.Unlock()
	data := createTestTorrentBytes(4, "registration-failed", []metainfo.FileInfo{{Length: 4, Path: []string{"file"}}}, [][]byte{[]byte("data")})
	mi, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddStagedTorrentFile(bytes.NewReader(data)); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	e.admissionMu.Lock()
	pending := len(e.registering) + len(e.detaching)
	e.registerMetainfo = nil
	e.admissionMu.Unlock()
	if pending != 0 || e.GetTorrent(mi.HashInfoBytes().HexString()) != nil {
		t.Fatal("failed registration leaked authority", pending)
	}
	if _, err := e.AddStagedTorrentFile(bytes.NewReader(data)); err != nil {
		t.Fatal("failed registration permanently occupied namespace", err)
	}
}
