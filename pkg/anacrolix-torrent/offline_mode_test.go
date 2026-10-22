package torrent

import (
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

func TestOfflineTorrentHasNoDiscoveryUntilEnabled(t *testing.T) {
	cfg := NewDefaultClientConfig()
	cfg.DataDir = t.TempDir()
	cfg.NoDHT = true
	cfg.DisableTCP = true
	cfg.DisableUTP = true
	cfg.NoDefaultPortForwarding = true
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	tor, _, err := client.AddTorrentSpec(&TorrentSpec{AddTorrentOpts: AddTorrentOpts{InfoHash: metainfo.Hash{1}, DisableNetworking: true, DisallowDataDownload: true, DisallowDataUpload: true}})
	if err != nil {
		t.Fatal(err)
	}
	if tor.NetworkingEnabled() || tor.DataDownloadAllowed() {
		t.Fatal("offline torrent activated")
	}
	if _, _, err := tor.AnnounceToDht(nil); err == nil {
		t.Fatal("explicit DHT announce bypassed offline mode")
	}
	client.lock()
	if tor.wantAnyConns() {
		client.unlock()
		t.Fatal("offline torrent wants connections")
	}
	client.unlock()
	tor.EnableNetworking()
	if !tor.NetworkingEnabled() || tor.DataDownloadAllowed() {
		t.Fatal("enabling discovery changed independent payload gate")
	}
}
