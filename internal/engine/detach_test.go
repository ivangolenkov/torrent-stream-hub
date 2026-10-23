package engine

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

func TestDetachDropDoesNotHoldAdmissionAndReservesOldNamespace(t *testing.T) {
	e, a := gateFixture(t)
	data := createTestTorrentBytes(4, "detach-independent", []metainfo.FileInfo{{Length: 4, Path: []string{"file"}}}, [][]byte{[]byte("data")})
	b, err := e.AddTorrentFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	e.admissionMu.Lock()
	e.dropTorrent = func(raw *torrent.Torrent) { close(entered); <-release; raw.Drop() }
	e.admissionMu.Unlock()
	deleted := make(chan error, 1)
	go func() { deleted <- e.Delete(a.hash) }()
	await(t, entered)
	paused := make(chan error, 1)
	go func() { paused <- e.Pause(b.Hash) }()
	select {
	case err := <-paused:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		unblock()
		t.Fatal("independent Pause waited on Drop")
	}
	if err := e.Delete(a.hash); !errors.Is(err, ErrOperationConflict) {
		t.Fatal("second deletion ignored pending Drop", err)
	}
	data = createTestTorrentBytes(4, "gate", []metainfo.FileInfo{{Length: 8, Path: []string{"video"}}}, [][]byte{[]byte("gooddata")})
	if _, err := e.AddTorrentFile(bytes.NewReader(data)); !errors.Is(err, ErrOperationConflict) {
		t.Fatal("re-add escaped detaching namespace reservation", err)
	}
	if e.GetTorrent(a.hash) != nil {
		t.Fatal("detaching generation republished")
	}
	unblock()
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	e.admissionMu.Lock()
	pending := len(e.detaching)
	e.dropTorrent = nil
	e.admissionMu.Unlock()
	if pending != 0 {
		t.Fatal("detach reservation leaked", pending)
	}
	if _, err := e.AddTorrentFile(bytes.NewReader(data)); err != nil {
		t.Fatal("re-add denied after Drop completed", err)
	}
}

type blockedDirectCloser struct{ entered, release chan struct{} }

func (c blockedDirectCloser) Close() error { close(c.entered); <-c.release; return nil }

func TestDetachDirectCloserRunsOutsideAdmission(t *testing.T) {
	e, a := gateFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	_, unregister, err := e.streamManager.RegisterDirectStream(a.hash, a, blockedDirectCloser{entered, release})
	if err != nil {
		t.Fatal(err)
	}
	defer unregister()
	finished := make(chan struct{})
	go func() { e.streamManager.CloseStreamsForTorrent(a); close(finished) }()
	await(t, entered)
	paused := make(chan error, 1)
	go func() { paused <- e.Pause(a.hash) }()
	select {
	case err := <-paused:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		unblock()
		t.Fatal("descriptor close retained admission lock")
	}
	unblock()
	await(t, finished)
}
