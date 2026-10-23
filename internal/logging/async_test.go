package logging

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func waitLogSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("logging worker did not progress")
	}
}

type blockedConsole struct {
	entered, release chan struct{}
	once             sync.Once
}

func (w *blockedConsole) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

func TestAsyncFilesBlockedConsoleDoesNotBlockFileOrProducer(t *testing.T) {
	w := &blockedConsole{entered: make(chan struct{}), release: make(chan struct{})}
	a, err := NewAsyncFiles(FileOptions{Dir: t.TempDir(), Console: w, RetentionDays: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	defer close(w.release)
	a.Write([]byte("first\n"))
	waitLogSignal(t, w.entered)
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		for range 2000 {
			a.Write([]byte("token=secret\n"))
		}
	}()
	waitLogSignal(t, producerDone)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.file.flush(ctx); err != nil {
		t.Fatal(err)
	}
	stats := a.Stats()
	if stats.Console.Dropped == 0 || stats.Console.Queued > sinkQueueRecords || stats.Console.QueuedBytes > sinkQueueBytes {
		t.Fatal(stats)
	}
	r, err := a.OpenLogs(ctx, Selection{From: "2000-01-01", To: "9999-12-31"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	p, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(p), "first") || strings.Contains(string(p), "secret") {
		t.Fatal("missing output or unredacted secret")
	}
}

func TestSinkQueueBudgetsCopyFlushAndDeferredCleanup(t *testing.T) {
	entered, release, cleaned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var output []string
	q := newSinkQueue(func(p []byte) (int, error) {
		if len(output) == 0 {
			close(entered)
			<-release
		}
		output = append(output, string(p))
		return len(p), nil
	}, func() error { close(cleaned); return nil })
	q.enqueue([]byte("first"))
	waitLogSignal(t, entered)
	p := []byte("original")
	q.enqueue(p)
	copy(p, "modified")
	for range 100 {
		q.enqueue([]byte(strings.Repeat("x", maxLogRecordBytes)))
	}
	s := q.snapshot()
	if s.QueuedBytes > sinkQueueBytes || s.Dropped == 0 || !s.InFlight {
		t.Fatal(s)
	}
	q.stop()
	q.stop()
	q.enqueue([]byte("late"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.flush(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-cleaned:
		t.Fatal("closed resource during in-flight Write")
	default:
	}
	close(release)
	waitLogSignal(t, q.done)
	if output[1] != "original" {
		t.Fatal("borrowed caller buffer")
	}
	if s = q.snapshot(); s.Completed != s.Accepted || s.Queued != 0 || s.InFlight {
		t.Fatal(s)
	}
	select {
	case <-cleaned:
	default:
		t.Fatal("cleanup not completed")
	}
}

func TestAsyncCloseDeadlineDoesNotCloseInFlightSink(t *testing.T) {
	w := &blockedConsole{entered: make(chan struct{}), release: make(chan struct{})}
	a, err := NewAsyncFiles(FileOptions{Dir: t.TempDir(), Console: w})
	if err != nil {
		t.Fatal(err)
	}
	a.Write([]byte("blocked"))
	waitLogSignal(t, w.entered)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.CloseContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	a.Write([]byte("late"))
	if !a.Stats().Console.Closed || a.Stats().Console.Dropped != 1 {
		t.Fatal(a.Stats())
	}
	close(w.release)
	waitLogSignal(t, a.console.done)
	waitLogSignal(t, a.file.done)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAsyncTruncatesAfterSanitizationAndCountsWriteErrors(t *testing.T) {
	a, err := NewAsyncFiles(FileOptions{Dir: t.TempDir(), Console: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	p := a.prepare([]byte("token=secret " + strings.Repeat("я", maxLogRecordBytes)))
	if len(p) > maxLogRecordBytes || strings.Contains(string(p), "secret") || !strings.HasSuffix(string(p), "[truncated]\n") || a.Stats().Truncated != 1 {
		t.Fatal("incorrect sanitization/truncation")
	}
	q := newSinkQueue(func([]byte) (int, error) { return 0, io.ErrShortWrite }, nil)
	q.enqueue(p)
	q.stop()
	waitLogSignal(t, q.done)
	if q.snapshot().Errors != 1 {
		t.Fatal(q.snapshot())
	}
}

func TestSinkQueueConcurrentStopFlushAndWrites(t *testing.T) {
	q := newSinkQueue(io.Discard.Write, nil)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				q.enqueue([]byte("message"))
			}
			q.stop()
			_ = q.flush(context.Background())
		}()
	}
	wg.Wait()
	waitLogSignal(t, q.done)
	s := q.snapshot()
	if s.Accepted+s.Dropped != 2000 || s.Accepted != s.Completed {
		t.Fatal(s)
	}
}

func TestBlockedFileDoesNotBlockConsoleOrStats(t *testing.T) {
	a, err := NewAsyncFiles(FileOptions{Dir: t.TempDir(), Console: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.files.mu.Lock()
	producerDone := make(chan struct{})
	go func() { a.Write([]byte("[INFO] separate sinks\n")); close(producerDone) }()
	waitLogSignal(t, producerDone)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = a.console.flush(ctx)
	statsDone := make(chan struct{})
	go func() { _ = a.Stats(); close(statsDone) }()
	select {
	case <-statsDone:
	case <-ctx.Done():
		a.files.mu.Unlock()
		t.Fatal("stats waited for file lock")
	}
	a.files.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

func TestLateWritesCountSeverityAndShutdownRejection(t *testing.T) {
	q := newSinkQueue(io.Discard.Write, nil)
	q.stop()
	for _, level := range []string{"DEBUG", "INFO", "WARN", "ERROR"} {
		q.enqueue([]byte("[" + level + "] lost"))
	}
	q.enqueue([]byte("raw message"))
	waitLogSignal(t, q.done)
	s := q.snapshot()
	if s.RejectedAfterClose != 5 || s.DroppedByLevel != (DroppedLevels{Debug: 1, Info: 1, Warn: 1, Error: 1, Unknown: 1}) {
		t.Fatal(s)
	}
}
