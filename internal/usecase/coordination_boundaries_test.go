package usecase

import (
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"
)

type blockedTorrentInput struct {
	entered, release chan struct{}
	once             sync.Once
}

func (r *blockedTorrentInput) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	<-r.release
	return 0, io.EOF
}

func TestBlockedInputReaderDoesNotOwnApplicationLock(t *testing.T) {
	uc, _, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	b := temporaryFile(t, uc, "independent-input", false)
	r := &blockedTorrentInput{entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(r.release) }) }
	defer unblock()
	finished := make(chan struct{})
	go func() { defer close(finished); _, _ = uc.AddTorrentFile(r) }()
	<-r.entered
	command := make(chan error, 1)
	go func() { command <- uc.Pause(b.Hash) }()
	select {
	case err := <-command:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		unblock()
		t.Fatal("independent command waited for input Reader")
	}
	lease, err := uc.AcquireTransfer(b.Hash)
	if err != nil {
		t.Fatal(err)
	}
	lease()
	unblock()
	<-finished
}

func TestBlockedJSONEncodingDoesNotOwnApplicationLock(t *testing.T) {
	uc, _, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	b := temporaryFile(t, uc, "independent-json", false)
	sw := NewSyncWorker(uc)
	defer sw.Close()
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	sw.marshal = func(value any) ([]byte, error) { close(entered); <-release; return json.Marshal(value) }
	go func() { defer close(finished); sw.Refresh() }()
	<-entered
	command := make(chan error, 1)
	go func() { command <- uc.Pause(b.Hash) }()
	select {
	case err := <-command:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		unblock()
		t.Fatal("independent command waited for JSON")
	}
	lease, err := uc.AcquireTransfer(b.Hash)
	if err != nil {
		t.Fatal(err)
	}
	lease()
	unblock()
	<-finished
}
