package config

import (
	"os"
	"testing"
)

func TestGetEnv(t *testing.T) {
	// Set environment variable
	os.Setenv("TEST_ENV_KEY", "test_value")
	defer os.Unsetenv("TEST_ENV_KEY")

	// Test existing env
	if val := getEnv("TEST_ENV_KEY", "fallback"); val != "test_value" {
		t.Errorf("Expected 'test_value', got '%s'", val)
	}

	// Test fallback
	if val := getEnv("NON_EXISTENT_KEY", "fallback"); val != "fallback" {
		t.Errorf("Expected 'fallback', got '%s'", val)
	}
}

func TestGetEnvAsInt(t *testing.T) {
	// Set valid integer
	os.Setenv("TEST_ENV_INT", "123")
	defer os.Unsetenv("TEST_ENV_INT")

	if val := getEnvAsInt("TEST_ENV_INT", 0); val != 123 {
		t.Errorf("Expected 123, got %d", val)
	}

	// Test fallback for non-existent key
	if val := getEnvAsInt("NON_EXISTENT_INT", 42); val != 42 {
		t.Errorf("Expected 42, got %d", val)
	}

	// Test fallback for invalid integer
	os.Setenv("TEST_ENV_INVALID_INT", "abc")
	defer os.Unsetenv("TEST_ENV_INVALID_INT")

	if val := getEnvAsInt("TEST_ENV_INVALID_INT", 42); val != 42 {
		t.Errorf("Expected 42, got %d", val)
	}
}

func TestGetEnvAsInt64(t *testing.T) {
	os.Setenv("TEST_ENV_INT64", "1234567890123")
	defer os.Unsetenv("TEST_ENV_INT64")

	if val := getEnvAsInt64("TEST_ENV_INT64", 0); val != 1234567890123 {
		t.Errorf("Expected 1234567890123, got %d", val)
	}

	if val := getEnvAsInt64("NON_EXISTENT_INT64", 42); val != 42 {
		t.Errorf("Expected 42, got %d", val)
	}
}

func TestGetEnvAsBool(t *testing.T) {
	os.Setenv("TEST_ENV_BOOL_TRUE", "true")
	defer os.Unsetenv("TEST_ENV_BOOL_TRUE")

	if val := getEnvAsBool("TEST_ENV_BOOL_TRUE", false); val != true {
		t.Errorf("Expected true, got %v", val)
	}

	os.Setenv("TEST_ENV_BOOL_FALSE", "false")
	defer os.Unsetenv("TEST_ENV_BOOL_FALSE")

	if val := getEnvAsBool("TEST_ENV_BOOL_FALSE", true); val != false {
		t.Errorf("Expected false, got %v", val)
	}

	if val := getEnvAsBool("NON_EXISTENT_BOOL", true); val != true {
		t.Errorf("Expected true, got %v", val)
	}

	os.Setenv("TEST_ENV_INVALID_BOOL", "yes")
	defer os.Unsetenv("TEST_ENV_INVALID_BOOL")

	if val := getEnvAsBool("TEST_ENV_INVALID_BOOL", false); val != false {
		t.Errorf("Expected false, got %v", val)
	}
}

func TestApplyDefaultsSetsBTDefaults(t *testing.T) {
	cfg := &Config{}

	ApplyDefaults(cfg)

	if cfg.BTClientProfile != "qbittorrent" {
		t.Fatalf("expected qbittorrent profile, got %q", cfg.BTClientProfile)
	}
	if cfg.BTRetrackersMode != "append" {
		t.Fatalf("expected append retrackers mode, got %q", cfg.BTRetrackersMode)
	}
	if cfg.BTRetrackersFile != "/config/trackers.txt" {
		t.Fatalf("expected default retrackers file, got %q", cfg.BTRetrackersFile)
	}
	if cfg.BTDownloadProfile != "balanced" {
		t.Fatalf("expected balanced download profile, got %q", cfg.BTDownloadProfile)
	}
	if cfg.BTEstablishedConns != 200 || cfg.BTHalfOpenConns != 200 || cfg.BTTotalHalfOpen != 2000 {
		t.Fatalf("unexpected connection defaults: %+v", cfg)
	}
	if cfg.BTPeersLowWater != 700 || cfg.BTPeersHighWater != 2500 || cfg.BTDialRateLimit != 200 {
		t.Fatalf("unexpected peer discovery defaults: %+v", cfg)
	}
	if !cfg.BTSwarmWatchdogEnabled || cfg.BTSwarmCheckIntervalSec != 60 || cfg.BTSwarmRefreshCooldownSec != 180 {
		t.Fatalf("unexpected swarm watchdog defaults: %+v", cfg)
	}
	if cfg.BTSwarmMinConnectedPeers != 8 || cfg.BTSwarmMinConnectedSeeds != 2 || cfg.BTSwarmStalledSpeedBps != 32768 {
		t.Fatalf("unexpected swarm threshold defaults: %+v", cfg)
	}
	if cfg.BTSwarmStalledDurationSec != 180 || cfg.BTSwarmBoostConns != 400 || cfg.BTSwarmBoostDurationSec != 300 {
		t.Fatalf("unexpected swarm boost defaults: %+v", cfg)
	}
	if cfg.BTSwarmPeerDropRatio != 0.45 || cfg.BTSwarmSeedDropRatio != 0.45 || cfg.BTSwarmSpeedDropRatio != 0.35 {
		t.Fatalf("unexpected swarm trend defaults: %+v", cfg)
	}
	if cfg.BTSwarmPeakTTLSec != 1800 || !cfg.BTSwarmHardRefreshEnabled || cfg.BTSwarmAutoHardRefreshEnabled || cfg.BTSwarmHardRefreshCooldownSec != 900 {
		t.Fatalf("unexpected hard refresh defaults: %+v", cfg)
	}
	if cfg.BTSwarmHardRefreshAfterSoftFails != 1 || cfg.BTSwarmHardRefreshMinTorrentAgeSec != 60 {
		t.Fatalf("unexpected hard refresh thresholds: %+v", cfg)
	}
	if cfg.BTSwarmDegradationEpisodeTTLSec != 900 || cfg.BTSwarmRecoveryGraceSec != 180 {
		t.Fatalf("unexpected episode defaults: %+v", cfg)
	}
	if !cfg.BTClientRecycleEnabled || cfg.BTClientRecycleCooldownSec != 900 || cfg.BTClientRecycleAfterHardFails != 1 || cfg.BTClientRecycleAfterSoftFails != 1 || cfg.BTClientRecycleMinTorrentAgeSec != 60 || cfg.BTClientRecycleMinTorrents != 1 || cfg.BTClientRecycleMaxPerHour != 2 {
		t.Fatalf("unexpected client recycle defaults: %+v", cfg)
	}
}

func TestApplyDefaultsDownloadProfiles(t *testing.T) {
	cases := []struct {
		profile                       string
		established, half, total, low int
		high, dial                    int
	}{
		{profile: "torrserver", established: 100, half: 80, total: 800, low: 300, high: 1000, dial: 60},
		{profile: "balanced", established: 200, half: 200, total: 2000, low: 700, high: 2500, dial: 200},
		{profile: "aggressive", established: 200, half: 200, total: 2000, low: 700, high: 2500, dial: 200},
	}
	for _, tc := range cases {
		t.Run(tc.profile, func(t *testing.T) {
			cfg := &Config{BTDownloadProfile: tc.profile}
			ApplyDefaults(cfg)
			if cfg.BTEstablishedConns != tc.established || cfg.BTHalfOpenConns != tc.half || cfg.BTTotalHalfOpen != tc.total || cfg.BTPeersLowWater != tc.low || cfg.BTPeersHighWater != tc.high || cfg.BTDialRateLimit != tc.dial {
				t.Fatalf("unexpected profile defaults: %+v", cfg)
			}
		})
	}
}

func TestApplyDefaultsDownloadProfileOverrides(t *testing.T) {
	cfg := &Config{BTDownloadProfile: "torrserver", BTEstablishedConns: 321}

	ApplyDefaults(cfg)

	if cfg.BTEstablishedConns != 321 || cfg.BTHalfOpenConns != 80 {
		t.Fatalf("expected explicit value to override profile defaults: %+v", cfg)
	}
}

func TestPublicIPStatusRejectsPrivateIP(t *testing.T) {
	if got := PublicIPStatus("192.168.1.10", false); got != "invalid" {
		t.Fatalf("expected private IPv4 invalid, got %q", got)
	}
	if got := PublicIPStatus("8.8.8.8", false); got != "configured" {
		t.Fatalf("expected public IPv4 configured, got %q", got)
	}
}

func TestApplyDefaultsClampsPeerWatermarks(t *testing.T) {
	cfg := &Config{BTPeersLowWater: 200, BTPeersHighWater: 100}

	ApplyDefaults(cfg)

	if cfg.BTPeersHighWater != cfg.BTPeersLowWater {
		t.Fatalf("expected high water to be clamped to low water, got low=%d high=%d", cfg.BTPeersLowWater, cfg.BTPeersHighWater)
	}
}

func TestApplyDefaultsClampsSwarmRatios(t *testing.T) {
	cfg := &Config{BTSwarmPeerDropRatio: 2, BTSwarmSeedDropRatio: 0.001, BTSwarmSpeedDropRatio: -1}

	ApplyDefaults(cfg)

	if cfg.BTSwarmPeerDropRatio != 0.95 {
		t.Fatalf("expected peer ratio to clamp high, got %f", cfg.BTSwarmPeerDropRatio)
	}
	if cfg.BTSwarmSeedDropRatio != 0.05 {
		t.Fatalf("expected seed ratio to clamp low, got %f", cfg.BTSwarmSeedDropRatio)
	}
	if cfg.BTSwarmSpeedDropRatio != 0.35 {
		t.Fatalf("expected invalid speed ratio to fallback, got %f", cfg.BTSwarmSpeedDropRatio)
	}
}

func TestApplyDefaultsHardRefreshCooldownAtLeastSoftCooldown(t *testing.T) {
	cfg := &Config{BTSwarmRefreshCooldownSec: 600, BTSwarmHardRefreshCooldownSec: 300}

	ApplyDefaults(cfg)

	if cfg.BTSwarmHardRefreshCooldownSec != cfg.BTSwarmRefreshCooldownSec {
		t.Fatalf("expected hard cooldown to clamp to soft cooldown, got hard=%d soft=%d", cfg.BTSwarmHardRefreshCooldownSec, cfg.BTSwarmRefreshCooldownSec)
	}
}

func TestApplyDefaultsClientRecycleCooldownIndependent(t *testing.T) {
	cfg := &Config{BTSwarmHardRefreshCooldownSec: 900, BTClientRecycleCooldownSec: 300}

	ApplyDefaults(cfg)

	if cfg.BTClientRecycleCooldownSec != 300 {
		t.Fatalf("expected client recycle cooldown to remain independent, got %d", cfg.BTClientRecycleCooldownSec)
	}
}
