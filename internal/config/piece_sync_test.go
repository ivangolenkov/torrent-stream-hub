package config

import "testing"

func TestDisablePieceSyncConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		args []string
		want bool
	}{
		{"default", "", nil, true},
		{"env true", "true", nil, true},
		{"env false", "false", nil, false},
		{"cli false", "true", []string{"--bt-disable-piece-sync=false"}, false},
		{"cli true", "false", []string{"--bt-disable-piece-sync"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HUB_BT_DISABLE_PIECE_SYNC", tc.env)
			cfg := loadLoggingConfig(t, tc.args...)
			ApplyDefaults(cfg)
			if cfg.BTDisablePieceSync != tc.want || !cfg.BTDisablePieceSyncConfigured {
				t.Fatalf("disable piece sync = %v, configured = %v", cfg.BTDisablePieceSync, cfg.BTDisablePieceSyncConfigured)
			}
		})
	}
}

func TestDisablePieceSyncProgrammaticDefaults(t *testing.T) {
	cfg := &Config{}
	ApplyDefaults(cfg)
	if !cfg.BTDisablePieceSync {
		t.Fatal("zero-value config must disable automatic piece sync")
	}
	cfg = &Config{BTDisablePieceSync: false, BTDisablePieceSyncConfigured: true}
	ApplyDefaults(cfg)
	ApplyDefaults(cfg)
	if cfg.BTDisablePieceSync {
		t.Fatal("explicit false was overwritten by defaults")
	}
}
