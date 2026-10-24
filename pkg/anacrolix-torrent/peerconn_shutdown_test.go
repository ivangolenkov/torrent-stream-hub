package torrent

import (
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/go-quicktest/qt"
)

func TestServePeerRequestDuringTorrentClose(t *testing.T) {
	cl := newTestingClient(t)
	tor := cl.newTorrentForTesting()
	tor.info = &metainfo.Info{PieceLength: 1, Length: 1, Pieces: make([]byte, 20)}
	pc := cl.newConnection(nil, newConnectionOpts{network: "test"})
	pc.setTorrent(tor)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	// Hold the actual upload path outside the client lock, then close while
	// its storage refresh is in flight. The read will return a closed error.
	tor.storage = &storage.Torrent{TorrentImpl: storage.TorrentImpl{
		FreshComplete: func(int) bool { return true },
		RefreshFresh: func(int, int) {
			close(entered)
			<-release
		},
	}}
	r := Request{ChunkSpec: ChunkSpec{Length: 1}}
	pc.unreadPeerRequests = map[Request]struct{}{r: {}}
	pc.readyPeerRequests = map[Request][]byte{{Index: 1, ChunkSpec: ChunkSpec{Length: 1}}: {1}}
	cl.lock()
	tor.conns[pc] = struct{}{}
	tor.initPieceRequestOrder()
	cl.unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		cl.lock()
		defer cl.unlock()
		pc.servePeerRequest(r)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not reach storage refresh")
	}
	tor.Drop()
	cl.lock()
	qt.Check(t, qt.HasLen(pc.unreadPeerRequests, 0))
	qt.Check(t, qt.HasLen(pc.readyPeerRequests, 0))
	cl.unlock()
	unblock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("closed upload request did not retire")
	}
}
