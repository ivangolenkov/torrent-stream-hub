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
	if cfg.BTEstablishedConns != 50 || cfg.BTHalfOpenConns != 50 || cfg.BTTotalHalfOpen != 500 {
		t.Fatalf("unexpected connection defaults: %+v", cfg)
	}
	if cfg.BTPeersLowWater != 100 || cfg.BTPeersHighWater != 1000 || cfg.BTDialRateLimit != 20 {
		t.Fatalf("unexpected peer discovery defaults: %+v", cfg)
	}
}

func TestApplyDefaultsClampsPeerWatermarks(t *testing.T) {
	cfg := &Config{BTPeersLowWater: 200, BTPeersHighWater: 100}

	ApplyDefaults(cfg)

	if cfg.BTPeersHighWater != cfg.BTPeersLowWater {
		t.Fatalf("expected high water to be clamped to low water, got low=%d high=%d", cfg.BTPeersLowWater, cfg.BTPeersHighWater)
	}
}
