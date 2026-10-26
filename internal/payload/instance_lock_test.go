package payload

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestInstanceLockOwnership(t *testing.T) {
	dir := t.TempDir()
	first, err := LockInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	before, err := os.Stat(filepath.Join(dir, InstanceLockName))
	if err != nil {
		t.Fatal(err)
	}
	if other, err := LockInstance(dir); !errors.Is(err, ErrInstanceLocked) {
		if other != nil {
			other.Close()
		}
		t.Fatal("second opener admitted", err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err == nil {
		if other, err := LockInstance(alias); !errors.Is(err, ErrInstanceLocked) {
			if other != nil {
				other.Close()
			}
			t.Fatal("root alias bypassed lock", err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal("close not idempotent", err)
	}
	second, err := LockInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	after, err := os.Stat(filepath.Join(dir, InstanceLockName))
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("lock file was replaced or removed", err)
	}
}

func TestInstanceLockRejectsLinks(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(t.TempDir(), "foreign")
			if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			link := os.Link
			if kind == "symlink" {
				link = os.Symlink
			}
			if err := link(target, filepath.Join(dir, InstanceLockName)); err != nil {
				t.Skip("links unavailable", err)
			}
			if lock, err := LockInstance(dir); err == nil {
				lock.Close()
				t.Fatal("unsafe lock file admitted")
			}
			if data, err := os.ReadFile(target); err != nil || string(data) != "keep" {
				t.Fatal("foreign file changed", err)
			}
		})
	}
}

func TestInstanceLockProcessRelease(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(fmt.Sprint("crash=", crash), func(t *testing.T) {
			dir := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInstanceLockHelper$")
			cmd.Env = append(os.Environ(), "TSH_INSTANCE_LOCK_HELPER="+dir)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				if !waited {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			}()
			line, err := bufio.NewReader(stdout).ReadString('\n')
			if err != nil || line != "locked\n" {
				t.Fatalf("helper: %q, %v", line, err)
			}
			if lock, err := LockInstance(dir); !errors.Is(err, ErrInstanceLocked) {
				if lock != nil {
					lock.Close()
				}
				t.Fatal("separate process admitted", err)
			}
			if crash {
				_ = cmd.Process.Kill()
			} else {
				_ = stdin.Close()
			}
			err = cmd.Wait()
			waited = true
			if !crash && err != nil {
				t.Fatal(err)
			}
			lock, err := LockInstance(dir)
			if err != nil {
				t.Fatal("lock not released after process exit", err)
			}
			defer lock.Close()
		})
	}
}

func TestInstanceLockHelper(t *testing.T) {
	dir := os.Getenv("TSH_INSTANCE_LOCK_HELPER")
	if dir == "" {
		return
	}
	lock, err := LockInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	fmt.Fprintln(os.Stdout, "locked")
	_, _ = io.Copy(io.Discard, os.Stdin)
}
