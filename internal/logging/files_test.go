package logging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func testFiles(t *testing.T, opts FileOptions) *Files {
	t.Helper()
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	if opts.Console == nil {
		opts.Console = io.Discard
	}
	if opts.Errors == nil {
		opts.Errors = io.Discard
	}
	f, err := NewFiles(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func putLog(t *testing.T, dir, date, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "hub-"+date+".log"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func readLog(t *testing.T, f *Files, from, to string, tail int) string {
	t.Helper()
	r, err := f.OpenLogs(context.Background(), Selection{From: from, To: to, Tail: tail})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	p, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(p)
}

func TestDailyRotationAppendAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	now := time.Date(2026, 10, 20, 23, 59, 0, 0, time.Local)
	var console bytes.Buffer
	f := testFiles(t, FileOptions{Dir: dir, RetentionDays: 3, Console: &console, Now: func() time.Time { return now }})
	p := []byte("first token=secret\nstack trace\n")
	if n, err := f.Write(p); err != nil || n != len(p) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	now = now.Add(2 * time.Minute)
	_, _ = f.Write([]byte("next day\n"))
	_ = f.Close()
	_ = f.Close()
	g := testFiles(t, FileOptions{Dir: dir, RetentionDays: 3, Now: func() time.Time { return now }})
	_, _ = g.Write([]byte("restart\n"))
	if got := readLog(t, g, "2026-10-20", "2026-10-21", 0); got != "first token=<redacted>\nstack trace\nnext day\nrestart\n" {
		t.Fatal(got)
	}
	if strings.Contains(console.String(), "secret") || !strings.Contains(console.String(), "next day") {
		t.Fatal(console.String())
	}
	for path, mode := range map[string]os.FileMode{dir: 0700, filepath.Join(dir, "hub-2026-10-20.log"): 0600} {
		if runtime.GOOS == "windows" {
			break
		} // Windows does not expose POSIX permission bits.
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("permissions %s: %v %v", path, info, err)
		}
	}
}

func TestRetentionCalendarBoundaryAndForeignFiles(t *testing.T) {
	zone, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, days := range []int{0, 3} {
		t.Run(fmt.Sprint(days), func(t *testing.T) {
			dir := t.TempDir()
			now := time.Date(2026, 3, 9, 0, 15, 0, 0, zone)
			for _, date := range []string{"2026-03-05", "2026-03-06", "2026-03-08", "2026-03-09"} {
				putLog(t, dir, date, "x")
			}
			for _, name := range []string{"other.log", "hub-2026-02-30.log", "hub-2026-01-01.log.bak"} {
				if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(filepath.Join(dir, "hub-2026-01-02.log"), 0700); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "secret")
			if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(dir, "hub-2026-01-01.log")); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("symlinks require privileges: %v", err)
				}
				t.Fatal(err)
			}
			f := testFiles(t, FileOptions{Dir: dir, RetentionDays: days, Now: func() time.Time { return now }})
			if _, err := os.Stat(filepath.Join(dir, "hub-2026-03-05.log")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			_, err := os.Stat(filepath.Join(dir, "hub-2026-03-06.log"))
			if (err == nil) != (days == 3) {
				t.Fatalf("boundary: %v", err)
			}
			for _, name := range []string{"other.log", "hub-2026-02-30.log", "hub-2026-01-01.log.bak", "hub-2026-01-02.log", "hub-2026-01-01.log"} {
				if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.OpenLogs(context.Background(), Selection{From: "2026-01-01", To: "2026-01-01"}); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("symlink read: %v", err)
			}
		})
	}
}

func TestPeriodicCleanupWithoutWrites(t *testing.T) {
	dir := t.TempDir()
	f := testFiles(t, FileOptions{Dir: dir, CleanupInterval: 5 * time.Millisecond})
	putLog(t, dir, "2000-01-01", "old")
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatal("cleanup did not run")
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(dir, "hub-2000-01-01.log")); errors.Is(err, os.ErrNotExist) {
				_ = f.Close()
				return
			}
		}
	}
}

type failingLogFile struct{}

func (failingLogFile) Write([]byte) (int, error) { return 0, syscall.ENOSPC }
func (failingLogFile) Close() error              { return nil }

func TestFileFailureConsoleAndRecovery(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.Local)
	var console, diagnostics bytes.Buffer
	dir := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(dir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	f := testFiles(t, FileOptions{Dir: dir, Console: &console, Errors: &diagnostics, Now: func() time.Time { return now }})
	for i := 0; i < 10; i++ {
		_, _ = f.Write([]byte("fallback\n"))
	}
	if strings.Count(diagnostics.String(), "file logging unavailable") != 1 {
		t.Fatal(diagnostics.String())
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	_, _ = f.Write([]byte("recovered\n"))
	if got := readLog(t, f, "2026-10-20", "2026-10-20", 0); got != "recovered\n" {
		t.Fatal(got)
	}
	f.mu.Lock()
	_ = f.file.Close()
	f.file = failingLogFile{}
	f.mu.Unlock()
	_, _ = f.Write([]byte("disk full\n"))
	if !strings.Contains(diagnostics.String(), "no space left") || !strings.Contains(console.String(), "disk full") {
		t.Fatalf("%s / %s", &console, &diagnostics)
	}
	now = now.Add(2 * time.Minute)
	_, _ = f.Write([]byte("space available\n"))
	if got := readLog(t, f, "2026-10-20", "2026-10-20", 0); got != "recovered\nspace available\n" {
		t.Fatal(got)
	}
}

func TestConcurrentWritesSnapshotsAndCleanup(t *testing.T) {
	f := testFiles(t, FileOptions{RetentionDays: 3})
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_, _ = f.Write([]byte("message\n"))
				r, err := f.OpenLogs(context.Background(), Selection{From: "2000-01-01", To: "9999-12-31", Tail: 5})
				if err != nil {
					t.Error(err)
					return
				}
				_, err = io.Copy(io.Discard, r)
				_ = r.Close()
				if err != nil {
					t.Error(err)
					return
				}
				f.mu.Lock()
				f.cleanupLocked()
				f.mu.Unlock()
			}
		}()
	}
	wg.Wait()
	got := readLog(t, f, "2000-01-01", "9999-12-31", 0)
	if strings.Count(got, "message\n") != 800 {
		t.Fatalf("bad write count: %d", len(got))
	}
}

func TestSharedAdaptersSanitizeAndLevels(t *testing.T) {
	f := testFiles(t, FileOptions{})
	oldOutput, oldFlags, oldPrefix := log.Writer(), log.Flags(), log.Prefix()
	oldSlog := slog.Default()
	oldLevel := CurrentLevel()
	defaultLogger.mu.Lock()
	oldDestination := defaultLogger.logger.Writer()
	defaultLogger.mu.Unlock()
	t.Cleanup(func() {
		slog.SetDefault(oldSlog)
		log.SetOutput(oldOutput)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
		defaultLogger.mu.Lock()
		defaultLogger.logger.SetOutput(oldDestination)
		defaultLogger.mu.Unlock()
		Configure(oldLevel)
	})
	UseFiles(f)
	Configure("info")
	Infof("app token=secret")
	log.Printf("standard passkey=secret")
	slog.Warn("library token=secret", "component", "torrent")
	slog.Default().WithGroup("peer").With(slog.Group("credentials", slog.String("password", "hidden with spaces"))).Info("grouped")
	slog.Debug("hidden debug")
	got := readLog(t, f, "2000-01-01", "9999-12-31", 0)
	if strings.Contains(got, "secret") || strings.Contains(got, "hidden with spaces") || strings.Contains(got, "hidden debug") || !strings.Contains(got, "[WARN] library") || strings.Count(got, "<redacted>") != 4 {
		t.Fatal(got)
	}
}

func TestSymlinkWriterDoesNotTouchOutsideFile(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "private")
	if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	name := "hub-" + time.Now().Format(dateLayout) + ".log"
	if err := os.Symlink(outside, filepath.Join(dir, name)); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks require privileges: %v", err)
		}
		t.Fatal(err)
	}
	var console, diagnostic bytes.Buffer
	f := testFiles(t, FileOptions{Dir: dir, Console: &console, Errors: &diagnostic})
	_, _ = f.Write([]byte("console only\n"))
	p, err := os.ReadFile(outside)
	if err != nil || string(p) != "untouched" || console.String() != "console only\n" || diagnostic.Len() == 0 {
		t.Fatalf("%q %v %q %q", p, err, &console, &diagnostic)
	}
}

func TestCleanupFailureIsReportedAndConsoleSurvives(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires unprivileged POSIX directory permissions")
	}
	dir := t.TempDir()
	putLog(t, dir, "2000-01-01", "old")
	var console, diagnostic bytes.Buffer
	f := testFiles(t, FileOptions{Dir: dir, RetentionDays: 36500, Console: &console, Errors: &diagnostic})
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	f.mu.Lock()
	f.opts.RetentionDays = 0
	f.cleanupLocked()
	f.mu.Unlock()
	if diagnostic.Len() == 0 {
		t.Fatal("cleanup failure was not reported")
	}
	_, _ = f.Write([]byte("still running\n"))
	if console.String() != "still running\n" {
		t.Fatal(console.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "hub-2000-01-01.log")); err != nil {
		t.Fatal(err)
	}
}
