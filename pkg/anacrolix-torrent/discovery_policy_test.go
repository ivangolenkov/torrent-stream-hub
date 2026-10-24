package torrent

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/torrent/metainfo"
)

type policyDHT struct {
	DhtServer
	calls    atomic.Int32
	announce *policyAnnounce
}

func (s *policyDHT) Announce([20]byte, int, bool) (DhtAnnounce, error) {
	s.calls.Add(1)
	return s.announce, nil
}

type policyAnnounce struct {
	peers  chan dht.PeersValues
	closed chan struct{}
	once   sync.Once
}

func (a *policyAnnounce) Peers() <-chan dht.PeersValues { return a.peers }
func (a *policyAnnounce) Close()                        { a.once.Do(func() { close(a.closed); close(a.peers) }) }

func TestPrivateMetadataCancelsDHTAndRejectsPublicPeers(t *testing.T) {
	cl := newTestingClient(t)
	tor := cl.newTorrentForTesting()
	ann := &policyAnnounce{peers: make(chan dht.PeersValues), closed: make(chan struct{})}
	s := &policyDHT{announce: ann}
	done, stop, err := tor.AnnounceToDht(s)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	private := true
	if err := tor.setInfoUnlocked(&metainfo.Info{Name: "private", Length: 1, PieceLength: 1, Pieces: make([]byte, 20), Private: &private}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("active announce survived private metadata")
	}
	select {
	case <-ann.closed:
	default:
		t.Fatal("DHT announce not closed")
	}
	if tor.PublicDiscoveryAllowed() {
		t.Fatal("private permits public discovery")
	}
	if _, _, err := tor.AnnounceToDht(s); err == nil || s.calls.Load() != 1 {
		t.Fatal("manual announce bypassed private policy", err, s.calls.Load())
	}
	cl.lock()
	defer cl.unlock()
	for _, source := range []PeerSource{PeerSourcePex, PeerSourceDhtGetPeers, PeerSourceDhtAnnouncePeer} {
		if tor.addPeer(PeerInfo{Source: source}) {
			t.Fatal("accepted late public peer")
		}
	}
	pex := pexConnState{enabled: true, torrent: tor}
	if pex.IsEnabled() {
		t.Fatal("private PEX enabled")
	}
	if err := pex.Recv([]byte("invalid")); err != nil {
		t.Fatal("private PEX parsed message", err)
	}
}

func TestPrivateSafeMagnetDisablesDiscoveryBeforeMetadata(t *testing.T) {
	spec, err := TorrentSpecFromMagnetUri("magnet:?xt=urn:btih:0000000000000000000000000000000000000001&x.hub-private=1")
	if err != nil {
		t.Fatal(err)
	}
	cl := newTestingClient(t)
	tor, _, err := cl.AddTorrentSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	s := &policyDHT{}
	if _, _, err := tor.AnnounceToDht(s); err == nil || s.calls.Load() != 0 {
		t.Fatal("private-safe leaked announce")
	}
}
