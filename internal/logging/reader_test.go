package logging

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestTailPhysicalLines(t *testing.T) {
	for _, text := range []string{"", "a", "a\n", "\n", "\n\n", "a\nb", "a\n\nb\n", strings.Repeat("x", 100000) + "\ntail\n"} {
		for n := 1; n <= 5; n++ {
			want := text
			lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
			if len(lines) > n {
				want = strings.Join(lines[len(lines)-n:], "\n")
				if strings.HasSuffix(text, "\n") {
					want += "\n"
				}
			}
			offset, err := tailOffset(context.Background(), strings.NewReader(text), int64(len(text)), n)
			if err != nil {
				t.Fatal(err)
			}
			if got := text[offset:]; got != want {
				t.Fatalf("tail %d offset %d text length %d", n, offset, len(text))
			}
		}
	}
}

func TestSelectionAndCrossFileTail(t *testing.T) {
	f := testFiles(t, FileOptions{RetentionDays: 36500})
	putLog(t, f.opts.Dir, "2026-10-18", "one\ntwo")
	putLog(t, f.opts.Dir, "2026-10-19", "")
	putLog(t, f.opts.Dir, "2026-10-20", "three\nfour\n")
	for n, want := range map[int]string{0: "one\ntwo\nthree\nfour\n", 1: "four\n", 3: "two\nthree\nfour\n", 10: "one\ntwo\nthree\nfour\n"} {
		if got := readLog(t, f, "2026-10-18", "2026-10-20", n); got != want {
			t.Fatalf("tail %d = %q", n, got)
		}
	}
	if got := readLog(t, f, "2026-10-19", "2026-10-19", 5); got != "" {
		t.Fatal(got)
	}
	if _, err := f.OpenLogs(context.Background(), Selection{From: "2000-01-01", To: "2000-01-01"}); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestSnapshotPinsSizeAndRetentionWithoutBlockingWriter(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.Local)
	f := testFiles(t, FileOptions{Now: func() time.Time { return now }})
	_, _ = f.Write([]byte("original\n"))
	r, err := f.OpenLogs(context.Background(), Selection{From: "2026-10-20", To: "2026-10-20"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("later\n"))
	now = now.AddDate(0, 0, 1)
	_, _ = f.Write([]byte("new day\n"))
	p, err := io.ReadAll(r)
	if err != nil || string(p) != "original\n" {
		t.Fatalf("snapshot %q %v", p, err)
	}
	if got := readLog(t, f, "2026-10-20", "2026-10-20", 0); got != "original\nlater\n" {
		t.Fatal(got)
	}
	_ = r.Close()
	_ = r.Close()
	f.mu.Lock()
	f.cleanupLocked()
	f.mu.Unlock()
	if _, err := f.OpenLogs(context.Background(), Selection{From: "2026-10-20", To: "2026-10-20"}); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestReaderCancellationAndClosedStore(t *testing.T) {
	f := testFiles(t, FileOptions{})
	_, _ = f.Write([]byte("line\n"))
	ctx, cancel := context.WithCancel(context.Background())
	r, err := f.OpenLogs(ctx, Selection{From: "2000-01-01", To: "9999-12-31"})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := io.ReadAll(r); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_ = r.Close()
	if _, err := f.OpenLogs(ctx, Selection{From: "2000-01-01", To: "9999-12-31"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := f.OpenLogs(context.Background(), Selection{}); !errors.Is(err, os.ErrClosed) {
		t.Fatal(err)
	}
	if len(f.refs) != 0 {
		t.Fatal(f.refs)
	}
}

type countingReader struct {
	io.ReaderAt
	bytes int
}

func (r *countingReader) ReadAt(p []byte, offset int64) (int, error) {
	n, err := r.ReaderAt.ReadAt(p, offset)
	r.bytes += n
	return n, err
}

func TestTailReadsOnlyEndOfLargeHistory(t *testing.T) {
	p := bytes.Repeat([]byte("old line\n"), 1000000)
	p = append(p, []byte("last\n")...)
	r := &countingReader{ReaderAt: bytes.NewReader(p)}
	offset, err := tailOffset(context.Background(), r, int64(len(p)), 1)
	if err != nil || string(p[offset:]) != "last\n" || r.bytes > 32769 {
		t.Fatalf("offset=%d bytes=%d err=%v", offset, r.bytes, err)
	}
}

func BenchmarkTailLongLine(b *testing.B) {
	r := strings.NewReader(strings.Repeat("x", 4*1024*1024))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := tailOffset(context.Background(), r, r.Size(), 1); err != nil {
			b.Fatal(err)
		}
	}
}
