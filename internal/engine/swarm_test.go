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
