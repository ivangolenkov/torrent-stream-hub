package logging

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSizeRotationReopenAndTailOrder(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	opts := FileOptions{Dir: dir, Console: io.Discard, RetentionDays: 3, Now: func() time.Time { return now }, MaxFileBytes: 8, MaxTotalBytes: 1024}
	f := testFiles(t, opts)
	for i := 0; i < 15; i++ {
		f.Write([]byte(fmt.Sprintf("%07d\n", i)))
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	g := testFiles(t, opts)
	g.Write([]byte("0000015\n"))
	if got := readLog(t, g, "2026-10-22", "2026-10-22", 3); got != "0000013\n0000014\n0000015\n" {
		t.Fatal(got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 16 {
		t.Fatal("restart overwrote segments", len(entries))
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || info.Size() > 8 {
			t.Fatal(info, err)
		}
	}
}

func TestQuotaPreservesPinnedSnapshotAndResumes(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	f := testFiles(t, FileOptions{RetentionDays: 3, Now: func() time.Time { return now }, MaxFileBytes: 4, MaxTotalBytes: 8})
	f.Write([]byte("one\n"))
	r, err := f.OpenLogs(context.Background(), Selection{From: "2026-10-22", To: "2026-10-22"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	f.Write([]byte("two\n"))
	f.Write([]byte("tri\n")) // Unpinned second segment is evicted.
	p, err := io.ReadAll(r)
	if err != nil || string(p) != "one\n" {
		t.Fatal(string(p), err)
	}
	other, err := f.OpenLogs(context.Background(), Selection{From: "2026-10-22", To: "2026-10-22"})
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("for\n")) // Both four-byte segments pinned; append must pause.
	if !f.StorageStats().QuotaPaused || f.StorageStats().LostRecords != 1 {
		t.Fatal(f.StorageStats())
	}
	other.Close()
	r.Close()
	f.Write([]byte("res\n"))
	if f.StorageStats().QuotaPaused {
		t.Fatal("quota did not recover")
	}
	if got := readLog(t, f, "2026-10-22", "2026-10-22", 0); !strings.HasSuffix(got, "res\n") {
		t.Fatal(got)
	}
}

func TestAsyncBacklogUsesEventDate(t *testing.T) {
	now := time.Date(2026, 10, 22, 23, 59, 0, 0, time.UTC)
	a, err := NewAsyncFiles(FileOptions{Dir: t.TempDir(), Console: io.Discard, RetentionDays: 3, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// Block the actual file worker before enqueue; the producer must still finish.
	a.files.mu.Lock()
	a.Write([]byte("yesterday\n"))
	// Clock mutation must not race worker retry-clock reads after release.
	now = now.Add(2 * time.Minute)
	a.Write([]byte("today\n"))
	a.files.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	for date, text := range map[string]string{"2026-10-22": "yesterday\n", "2026-10-23": "today\n"} {
		r, err := a.OpenLogs(ctx, Selection{From: date, To: date})
		if err != nil {
			t.Fatal(err)
		}
		p, err := io.ReadAll(r)
		r.Close()
		if err != nil || string(p) != text {
			t.Fatal(string(p), err)
		}
	}
}

func TestQuotaDoesNotGrowOversizedLegacyOrTouchForeignFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	putLog(t, dir, "2026-10-22", "old oversized legacy\n")
	foreign := filepath.Join(dir, "unmanaged.log")
	if err := os.WriteFile(foreign, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	f := testFiles(t, FileOptions{Dir: dir, RetentionDays: 3, Now: func() time.Time { return now }, MaxFileBytes: 4, MaxTotalBytes: 64})
	f.Write([]byte("new\n"))
	if p, err := os.ReadFile(filepath.Join(dir, "hub-2026-10-22.log")); err != nil || string(p) != "old oversized legacy\n" {
		t.Fatal(string(p), err)
	}
	if p, err := os.ReadFile(foreign); err != nil || string(p) != "foreign" {
		t.Fatal(string(p), err)
	}
}
