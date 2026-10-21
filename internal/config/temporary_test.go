package config

import (
	"os"
	"testing"
)

func TestTemporaryConfig(t *testing.T) {
	for _, tc := range []struct {
		env     string
		args    []string
		want    int
		invalid bool
	}{
		{"", nil, 3, false},
		{"8", nil, 8, false},
		{"bad", []string{"--max-temporary-torrents", "2"}, 2, false},
		{"0", nil, 0, true},
		{"-1", nil, 0, true},
		{"bad", nil, 0, true},
		{"999999999999999999999999999999", nil, 0, true},
		{"5", []string{"--max-temporary-torrents", "0"}, 0, true},
	} {
		t.Run(tc.env+"/"+string(rune(tc.want+'0')), func(t *testing.T) {
			t.Setenv("HUB_MAX_TEMPORARY_TORRENTS", tc.env)
			if tc.env == "" {
				if err := os.Unsetenv("HUB_MAX_TEMPORARY_TORRENTS"); err != nil {
					t.Fatal(err)
				}
			}
			cfg := loadLoggingConfig(t, tc.args...)
			if (cfg.ValidateTemporaryTorrents() != nil) != tc.invalid {
				t.Fatalf("unexpected validation: %v", cfg.ValidateTemporaryTorrents())
			}
			if !tc.invalid && cfg.MaxTemporaryTorrents != tc.want {
				t.Fatalf("got %d want %d", cfg.MaxTemporaryTorrents, tc.want)
			}
		})
	}
}
