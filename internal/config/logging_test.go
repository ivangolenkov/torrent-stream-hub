package config

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

func loadLoggingConfig(t *testing.T, args ...string) *Config {
	t.Helper()
	oldFlags, oldArgs := flag.CommandLine, os.Args
	flag.CommandLine = flag.NewFlagSet("test", flag.ContinueOnError)
	os.Args = append([]string{"test"}, args...)
	t.Cleanup(func() { flag.CommandLine, os.Args = oldFlags, oldArgs })
	return Load()
}

func TestLoggingDefaultsAndCLIPrecedence(t *testing.T) {
	t.Setenv("HUB_LOG_DIR", "")
	t.Setenv("HUB_LOG_RETENTION_DAYS", "3")
	cfg := loadLoggingConfig(t, "--db-path", filepath.Join("custom", "hub.db"))
	if cfg.LogDir != filepath.Join("custom", "logs") || cfg.LogRetentionDays != 3 {
		t.Fatalf("%s %d", cfg.LogDir, cfg.LogRetentionDays)
	}
	if cfg.LogMaxFileBytes != 16<<20 || cfg.LogMaxTotalBytes != 128<<20 {
		t.Fatal("unexpected log byte defaults")
	}
}

func TestLoggingByteLimitValidation(t *testing.T) {
	for _, raw := range []string{"0", "-1", "text", "9223372036854775808", "1024"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("HUB_LOG_MAX_FILE_BYTES", raw)
			if cfg := loadLoggingConfig(t); cfg.ValidateLogging() == nil {
				t.Fatal("invalid byte limit accepted")
			}
		})
	}
	t.Run("cli overrides invalid env", func(t *testing.T) {
		t.Setenv("HUB_LOG_MAX_FILE_BYTES", "bad")
		cfg := loadLoggingConfig(t, "--log-max-file-bytes", "65536", "--log-max-total-bytes", "131072")
		if cfg.ValidateLogging() != nil || cfg.LogMaxFileBytes != 65536 || cfg.LogMaxTotalBytes != 131072 {
			t.Fatal(cfg.ValidateLogging())
		}
	})
	t.Run("total below file", func(t *testing.T) {
		cfg := loadLoggingConfig(t, "--log-max-file-bytes", "131072", "--log-max-total-bytes", "65536")
		if cfg.ValidateLogging() == nil {
			t.Fatal("invalid total accepted")
		}
	})
}

func TestLoggingEnvironmentAndZeroRetention(t *testing.T) {
	t.Setenv("HUB_LOG_DIR", "env-logs")
	t.Setenv("HUB_LOG_RETENTION_DAYS", "5")
	cfg := loadLoggingConfig(t, "--log-dir", "cli-logs", "--log-retention-days", "0")
	if cfg.LogDir != "cli-logs" || cfg.LogRetentionDays != 0 || cfg.ValidateLogging() != nil {
		t.Fatalf("%s %d", cfg.LogDir, cfg.LogRetentionDays)
	}
	ApplyDefaults(cfg)
	if cfg.LogRetentionDays != 0 {
		t.Fatal("zero retention was lost")
	}
}

func TestLoggingRejectsNegativeRetention(t *testing.T) {
	t.Setenv("HUB_LOG_RETENTION_DAYS", "-1")
	cfg := loadLoggingConfig(t)
	if cfg.ValidateLogging() == nil {
		t.Fatal("negative retention accepted")
	}
}

func TestLoggingRejectsInvalidEnvironment(t *testing.T) {
	t.Setenv("HUB_LOG_RETENTION_DAYS", "not-an-integer")
	cfg := loadLoggingConfig(t)
	if cfg.ValidateLogging() == nil {
		t.Fatal("invalid retention accepted")
	}
}
