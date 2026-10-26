package config

import (
	"flag"
	"os"
	"testing"
)

func TestResumeConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		set       bool
		args      []string
		want      string
		invalid   bool
	}{
		{name: "upgrade-default", want: "fast"},
		{name: "strict-env", env: "verify", set: true, want: "verify"},
		{name: "fast-env", env: "fast", set: true, want: "fast"},
		{name: "unknown", env: "FAST", set: true, invalid: true},
		{name: "empty", set: true, invalid: true},
		{name: "cli-override", env: "invalid", set: true, args: []string{"--fast-resume-mode=verify"}, want: "verify"},
		{name: "empty-cli", args: []string{"--fast-resume-mode="}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldFlags, oldArgs := flag.CommandLine, os.Args
			defer func() { flag.CommandLine, os.Args = oldFlags, oldArgs }()
			t.Setenv("HUB_FAST_RESUME_MODE", tc.env)
			if !tc.set {
				_ = os.Unsetenv("HUB_FAST_RESUME_MODE")
			}
			flag.CommandLine = flag.NewFlagSet("resume-test", flag.ContinueOnError)
			os.Args = append([]string{"resume-test"}, tc.args...)
			cfg := Load()
			if err := cfg.ValidateResources(); (err != nil) != tc.invalid {
				t.Fatalf("validation: %v", err)
			}
			if !tc.invalid && cfg.FastResumeMode != tc.want {
				t.Fatalf("mode %q != %q", cfg.FastResumeMode, tc.want)
			}
		})
	}
}
