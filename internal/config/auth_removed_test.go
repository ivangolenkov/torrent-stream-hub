package config

import (
	"flag"
	"io"
	"testing"
)

func TestAuthenticationOptionsRemoved(t *testing.T) {
	t.Setenv("HUB_AUTH_ENABLED", "true")
	t.Setenv("HUB_AUTH_USER", "old-user")
	t.Setenv("HUB_AUTH_PASSWORD", "old-password")
	_ = loadLoggingConfig(t)
	flag.CommandLine.SetOutput(io.Discard)
	for _, name := range []string{"auth-enabled", "auth-user", "auth-password"} {
		if flag.Lookup(name) != nil {
			t.Fatalf("obsolete option %s registered", name)
		}
		if err := flag.CommandLine.Parse([]string{"--" + name + "=test"}); err == nil {
			t.Fatalf("obsolete option %s accepted", name)
		}
	}
}
