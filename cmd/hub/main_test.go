package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartupFailureSavedBeforeClosingLogger(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocked, nil, 0600); err != nil {
		t.Fatal(err)
	}
	oldFlags, oldArgs := flag.CommandLine, os.Args
	flag.CommandLine = flag.NewFlagSet("test", flag.ContinueOnError)
	os.Args = []string{"test", "--download-dir", blocked, "--db-path", filepath.Join(dir, "hub.db")}
	t.Cleanup(func() { flag.CommandLine, os.Args = oldFlags, oldArgs })
	t.Setenv("HUB_LOG_DIR", filepath.Join(dir, "logs"))
	t.Setenv("HUB_LOG_LEVEL", "info")
	t.Setenv("HUB_LOG_RETENTION_DAYS", "3")
	if err := run(); err == nil {
		t.Fatal("expected startup failure")
	}
	paths, err := filepath.Glob(filepath.Join(dir, "logs", "hub-*.log"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("%v %v", paths, err)
	}
	p, err := os.ReadFile(paths[0])
	if err != nil || !strings.Contains(string(p), "failed to prepare runtime directories") {
		t.Fatalf("%s %v", p, err)
	}
}
