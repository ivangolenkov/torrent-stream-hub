package engine

import (
	"testing"
	"time"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/models"
)

func testSwarmConfig() *config.Config {
	cfg := &config.Config{}
	config.ApplyDefaults(cfg)
	return cfg
}

func TestDecideSwarmHealthLowConnectedPeers(t *testing.T) {
	cfg := testSwarmConfig()
	now := time.Now()

	decision := decideSwarmHealth(cfg, swarmSnapshot{
		State:         models.StateDownloading,
		MetadataReady: true,
		Connected:     cfg.BTSwarmMinConnectedPeers - 1,
		Seeds:         cfg.BTSwarmMinConnectedSeeds,
		DownloadSpeed: int64(cfg.BTSwarmStalledSpeedBps * 2),
		Now:           now,
	}, time.Time{})

	if !decision.Degraded || decision.Reason != "connected peers below threshold" {
		t.Fatalf("expected low peers degradation, got %+v", decision)
	}
}

func TestDecideSwarmHealthLowConnectedSeeds(t *testing.T) {
	cfg := testSwarmConfig()
	now := time.Now()

	decision := decideSwarmHealth(cfg, swarmSnapshot{
		State:         models.StateDownloading,
		MetadataReady: true,
		Connected:     cfg.BTSwarmMinConnectedPeers,
		Seeds:         cfg.BTSwarmMinConnectedSeeds - 1,
		DownloadSpeed: int64(cfg.BTSwarmStalledSpeedBps * 2),
		Now:           now,
	}, time.Time{})

	if !decision.Degraded || decision.Reason != "connected seeds below threshold" {
		t.Fatalf("expected low seeds degradation, got %+v", decision)
	}
}

func TestDecideSwarmHealthStalledDownload(t *testing.T) {
	cfg := testSwarmConfig()
	now := time.Now()
	stallStarted := now.Add(-time.Duration(cfg.BTSwarmStalledDurationSec+1) * time.Second)

	decision := decideSwarmHealth(cfg, swarmSnapshot{
		State:         models.StateDownloading,
		MetadataReady: true,
		Connected:     cfg.BTSwarmMinConnectedPeers,
		Seeds:         cfg.BTSwarmMinConnectedSeeds,
		DownloadSpeed: int64(cfg.BTSwarmStalledSpeedBps - 1),
		Now:           now,
	}, stallStarted)

	if !decision.Degraded || decision.Reason != "download speed stalled" {
		t.Fatalf("expected stalled degradation, got %+v", decision)
	}
}

func TestDecideSwarmHealthCompletedSeedingIgnoresDownloadSpeed(t *testing.T) {
	cfg := testSwarmConfig()
	now := time.Now()
	stallStarted := now.Add(-time.Duration(cfg.BTSwarmStalledDurationSec+1) * time.Second)

	decision := decideSwarmHealth(cfg, swarmSnapshot{
		State:         models.StateSeeding,
		MetadataReady: true,
		Complete:      true,
		Connected:     cfg.BTSwarmMinConnectedPeers,
		Seeds:         0,
		DownloadSpeed: 0,
		Now:           now,
	}, stallStarted)

	if decision.Degraded {
		t.Fatalf("expected completed seeding torrent to stay healthy, got %+v", decision)
	}
}

func TestDecideSwarmHealthPausedIgnored(t *testing.T) {
	cfg := testSwarmConfig()

	decision := decideSwarmHealth(cfg, swarmSnapshot{
		State:         models.StatePaused,
		MetadataReady: true,
		Connected:     0,
		Seeds:         0,
		DownloadSpeed: 0,
		Now:           time.Now(),
	}, time.Now().Add(-time.Hour))

	if decision.Degraded {
		t.Fatalf("expected paused torrent to be ignored, got %+v", decision)
	}
}

func TestDecideSwarmHealthPeerDropBelowRecentPeak(t *testing.T) {
	cfg := testSwarmConfig()
	now := time.Now()

	decision := decideSwarmHealth(cfg, swarmSnapshot{
		State:         models.StateDownloading,
		MetadataReady: true,
		Connected:     30,
		Seeds:         cfg.BTSwarmMinConnectedSeeds,
		DownloadSpeed: int64(cfg.BTSwarmStalledSpeedBps * 2),
		PeakConnected: 100,
		PeakUpdatedAt: now.Add(-time.Minute),
		Now:           now,
	}, time.Time{})

	if !decision.Degraded || decision.Reason != "connected peers dropped below recent peak" {
		t.Fatalf("expected peer trend degradation, got %+v", decision)
	}
}

func TestDecideSwarmHealthExpiredPeakIgnored(t *testing.T) {
	cfg := testSwarmConfig()
	now := time.Now()

	decision := decideSwarmHealth(cfg, swarmSnapshot{
		State:         models.StateDownloading,
		MetadataReady: true,
		Connected:     cfg.BTSwarmMinConnectedPeers,
		Seeds:         cfg.BTSwarmMinConnectedSeeds,
		DownloadSpeed: int64(cfg.BTSwarmStalledSpeedBps * 2),
		PeakConnected: 100,
		PeakUpdatedAt: now.Add(-time.Duration(cfg.BTSwarmPeakTTLSec+1) * time.Second),
		Now:           now,
	}, time.Time{})

	if decision.Degraded {
		t.Fatalf("expected expired peak to be ignored, got %+v", decision)
	}
}

func TestDecideSwarmHealthCompletedSeedingIgnoresSpeedDropPeak(t *testing.T) {
	cfg := testSwarmConfig()
	now := time.Now()

	decision := decideSwarmHealth(cfg, swarmSnapshot{
		State:             models.StateSeeding,
		MetadataReady:     true,
		Complete:          true,
		Connected:         cfg.BTSwarmMinConnectedPeers,
		DownloadSpeed:     0,
		PeakDownloadSpeed: 10 * 1024 * 1024,
		PeakUpdatedAt:     now.Add(-time.Minute),
		Now:               now,
	}, time.Time{})

	if decision.Degraded {
		t.Fatalf("expected completed seeding torrent to ignore speed peak, got %+v", decision)
	}
}

func TestDecideHardRefreshBlockedReason(t *testing.T) {
	cfg := testSwarmConfig()
	now := time.Now()

	cases := []struct {
		name string
		snap hardRefreshGateSnapshot
		want string
	}{
		{name: "cooldown", snap: hardRefreshGateSnapshot{State: models.StateDownloading, AddedAt: now.Add(-time.Hour), LastHardRefreshAt: now.Add(-time.Minute), SoftRefreshCount: 10, Now: now}, want: "cooldown"},
		{name: "young", snap: hardRefreshGateSnapshot{State: models.StateDownloading, AddedAt: now.Add(-30 * time.Second), SoftRefreshCount: 10, Now: now}, want: "torrent too young"},
		{name: "active stream", snap: hardRefreshGateSnapshot{State: models.StateDownloading, AddedAt: now.Add(-time.Hour), SoftRefreshCount: 10, ActiveStreams: 1, Now: now}, want: "active stream"},
		{name: "soft fails", snap: hardRefreshGateSnapshot{State: models.StateDownloading, AddedAt: now.Add(-time.Hour), SoftRefreshCount: 0, Now: now}, want: "waiting for soft refresh attempts"},
		{name: "allowed", snap: hardRefreshGateSnapshot{State: models.StateDownloading, AddedAt: now.Add(-time.Hour), SoftRefreshCount: 10, Now: now}, want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideHardRefreshBlockedReason(cfg, tc.snap); got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}
