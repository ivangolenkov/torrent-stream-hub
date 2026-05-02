package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"torrent-stream-hub/internal/config"

	"github.com/anacrolix/torrent"
)

func TestBuildClientConfigAppliesBTDefaults(t *testing.T) {
	cfg := &config.Config{DownloadDir: t.TempDir(), TorrentPort: 0, BTDisableUTP: true}

	clientConfig := buildClientConfig(cfg)

	if !clientConfig.Seed {
		t.Fatalf("expected seeding to be enabled by default")
	}
	if clientConfig.NoUpload {
		t.Fatalf("expected upload to be enabled by default")
	}
	if clientConfig.NoDHT || clientConfig.DisablePEX || clientConfig.DisableTCP || clientConfig.NoDefaultPortForwarding {
		t.Fatalf("expected DHT/PEX/TCP/UPnP to be enabled by default")
	}
	if !clientConfig.DisableUTP {
		t.Fatalf("expected UTP to be disabled by default")
	}
	if clientConfig.EstablishedConnsPerTorrent != 200 || clientConfig.HalfOpenConnsPerTorrent != 200 || clientConfig.TotalHalfOpenConns != 2000 {
		t.Fatalf("unexpected connection defaults: %+v", clientConfig)
	}
	if clientConfig.TorrentPeersLowWater != 700 || clientConfig.TorrentPeersHighWater != 2500 {
		t.Fatalf("unexpected peer watermarks: low=%d high=%d", clientConfig.TorrentPeersLowWater, clientConfig.TorrentPeersHighWater)
	}
	if clientConfig.NominalDialTimeout != 5*time.Second || clientConfig.MinDialTimeout != 2*time.Second || clientConfig.HandshakesTimeout != 20*time.Second {
		t.Fatalf("unexpected timeouts: dial=%v min_dial=%v handshake=%v", clientConfig.NominalDialTimeout, clientConfig.MinDialTimeout, clientConfig.HandshakesTimeout)
	}
	if clientConfig.PieceHashersPerTorrent != 4 {
		t.Fatalf("unexpected piece hashers: %d", clientConfig.PieceHashersPerTorrent)
	}
}

func TestBuildClientConfigAppliesQBittorrentProfile(t *testing.T) {
	cfg := &config.Config{DownloadDir: t.TempDir(), BTClientProfile: "qbittorrent"}

	clientConfig := buildClientConfig(cfg)

	if clientConfig.HTTPUserAgent != "qBittorrent/4.3.9" {
		t.Fatalf("unexpected user agent: %q", clientConfig.HTTPUserAgent)
	}
	if clientConfig.ExtendedHandshakeClientVersion != "qBittorrent/4.3.9" {
		t.Fatalf("unexpected extended handshake client version: %q", clientConfig.ExtendedHandshakeClientVersion)
	}
	if clientConfig.Bep20 != "-qB4390-" {
		t.Fatalf("unexpected BEP20 prefix: %q", clientConfig.Bep20)
	}
	if !strings.HasPrefix(clientConfig.PeerID, "-qB4390-") || len(clientConfig.PeerID) != 20 {
		t.Fatalf("unexpected peer id: %q", clientConfig.PeerID)
	}
}

func TestRetrackersAppendReplaceAndOff(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		existing [][]string
		wantLen  int
	}{
		{name: "append", mode: "append", existing: [][]string{{"udp://existing.example:80/announce"}}, wantLen: 2},
		{name: "replace", mode: "replace", existing: [][]string{{"udp://existing.example:80/announce"}}, wantLen: 1},
		{name: "off", mode: "off", existing: [][]string{{"udp://existing.example:80/announce"}}, wantLen: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &Engine{cfg: &config.Config{BTRetrackersMode: tt.mode, BTRetrackersFile: filepath.Join(t.TempDir(), "missing.txt")}}
			spec := &torrent.TorrentSpec{Trackers: tt.existing}

			e.augmentTorrentSpec(spec)

			if len(spec.Trackers) != tt.wantLen {
				t.Fatalf("expected %d tracker tiers, got %d: %#v", tt.wantLen, len(spec.Trackers), spec.Trackers)
			}
			if tt.mode == "append" && spec.Trackers[0][0] != "udp://existing.example:80/announce" {
				t.Fatalf("expected existing tracker to be preserved first, got %#v", spec.Trackers)
			}
		})
	}
}

func TestRetrackersFileIsLoadedAndDeduplicated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trackers.txt")
	if err := os.WriteFile(path, []byte("udp://custom.example:80/announce\nnot-a-url\nudp://custom.example:80/announce\n"), 0o600); err != nil {
		t.Fatalf("failed to write trackers file: %v", err)
	}

	trackers := mergeTrackers(loadTrackersFile(path))

	if len(trackers) != 1 || trackers[0] != "udp://custom.example:80/announce" {
		t.Fatalf("expected one valid deduplicated tracker, got %#v", trackers)
	}
}
