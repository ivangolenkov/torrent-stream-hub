package engine

import (
	"testing"
	"time"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/models"
)

func testLowSpeedEngine() *Engine {
	cfg := &config.Config{
		BTSwarmStalledSpeedBps:    32768,
		BTSwarmStalledDurationSec: 60,
		BTSwarmRefreshCooldownSec: 120,
		BTSwarmSpeedDropRatio:     0.35,
		BTSwarmPeakTTLSec:         300,
	}
	config.ApplyDefaults(cfg)
	return &Engine{cfg: cfg}
}

func TestLowSpeedRefreshReasonBelowAbsoluteThreshold(t *testing.T) {
	e := testLowSpeedEngine()
	mt := &ManagedTorrent{state: models.StateDownloading, downloadSpeed: 1024}

	reason := e.lowSpeedRefreshReasonLocked(mt, true, false, time.Now())
	if reason != "stalled_speed_below_threshold" {
		t.Fatalf("expected absolute low-speed reason, got %q", reason)
	}
}

func TestLowSpeedRefreshReasonBelowRecentPeak(t *testing.T) {
	e := testLowSpeedEngine()
	now := time.Now()
	mt := &ManagedTorrent{
		state:             models.StateDownloading,
		downloadSpeed:     200 * 1024,
		peakDownloadSpeed: 1024 * 1024,
		peakUpdatedAt:     now.Add(-time.Minute),
	}

	reason := e.lowSpeedRefreshReasonLocked(mt, true, false, now)
	if reason != "speed_dropped_below_peak" {
		t.Fatalf("expected relative peak-drop reason, got %q", reason)
	}
}

func TestLowSpeedRefreshReasonIgnoresPausedTorrent(t *testing.T) {
	e := testLowSpeedEngine()
	mt := &ManagedTorrent{state: models.StatePaused, downloadSpeed: 0}

	reason := e.lowSpeedRefreshReasonLocked(mt, true, false, time.Now())
	if reason != "" {
		t.Fatalf("expected paused torrent to be ignored, got %q", reason)
	}
}

func TestLowSpeedRefreshReasonIgnoresExpiredPeak(t *testing.T) {
	e := testLowSpeedEngine()
	now := time.Now()
	mt := &ManagedTorrent{
		state:             models.StateDownloading,
		downloadSpeed:     200 * 1024,
		peakDownloadSpeed: 1024 * 1024,
		peakUpdatedAt:     now.Add(-10 * time.Minute),
	}

	reason := e.lowSpeedRefreshReasonLocked(mt, true, false, now)
	if reason != "" {
		t.Fatalf("expected expired peak to be ignored, got %q", reason)
	}
}

func TestUpdatePeakDownloadSpeedExpiresOldPeak(t *testing.T) {
	e := testLowSpeedEngine()
	now := time.Now()
	mt := &ManagedTorrent{
		state:             models.StateDownloading,
		downloadSpeed:     128 * 1024,
		peakDownloadSpeed: 1024 * 1024,
		peakUpdatedAt:     now.Add(-10 * time.Minute),
	}

	e.updatePeakDownloadSpeedLocked(mt, now)

	if mt.peakDownloadSpeed != mt.downloadSpeed {
		t.Fatalf("expected expired peak to reset to current speed, got peak=%d current=%d", mt.peakDownloadSpeed, mt.downloadSpeed)
	}
	if !mt.peakUpdatedAt.Equal(now) {
		t.Fatalf("expected peak timestamp to update to now, got %s", mt.peakUpdatedAt)
	}
}

func TestLowSpeedDurationsUseConfig(t *testing.T) {
	e := testLowSpeedEngine()

	if got := e.swarmStalledDuration(); got != time.Minute {
		t.Fatalf("expected stalled duration from config, got %s", got)
	}
	if got := e.swarmRefreshCooldown(); got != 2*time.Minute {
		t.Fatalf("expected refresh cooldown from config, got %s", got)
	}
}
