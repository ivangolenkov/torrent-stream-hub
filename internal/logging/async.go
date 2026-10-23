package logging

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	sinkQueueRecords  = 512
	sinkQueueBytes    = 2 << 20
	maxLogRecordBytes = 64 << 10
	logDrainTimeout   = 2 * time.Second
)

// SinkStats describes delivery, not durable storage. One record may be accepted
// by one sink and dropped by the other. InFlight is outside the waiting budget.
type SinkStats struct {
	Queued             int           `json:"queued"`
	QueuedBytes        int           `json:"queued_bytes"`
	InFlight           bool          `json:"in_flight"`
	Accepted           uint64        `json:"accepted"`
	Completed          uint64        `json:"completed"`
	Dropped            uint64        `json:"dropped"`
	DroppedBytes       uint64        `json:"dropped_bytes"`
	Errors             uint64        `json:"errors"`
	Closed             bool          `json:"closed"`
	RejectedAfterClose uint64        `json:"rejected_after_close"`
	DroppedByLevel     DroppedLevels `json:"dropped_by_level"`
}

type DroppedLevels struct {
	Debug   uint64 `json:"debug"`
	Info    uint64 `json:"info"`
	Warn    uint64 `json:"warn"`
	Error   uint64 `json:"error"`
	Unknown uint64 `json:"unknown"`
}

func (d *DroppedLevels) add(p []byte) {
	// Adapters emit a bounded timestamp prefix followed by [LEVEL]. Raw writer
	// clients without that envelope have no trustworthy severity information.
	p = p[:min(len(p), 64)]
	switch {
	case bytes.Contains(p, []byte("[DEBUG]")):
		d.Debug++
	case bytes.Contains(p, []byte("[INFO]")):
		d.Info++
	case bytes.Contains(p, []byte("[WARN]")):
		d.Warn++
	case bytes.Contains(p, []byte("[ERROR]")):
		d.Error++
	default:
		d.Unknown++
	}
}

// sinkQueue has one worker. Its mutex is never held across sink I/O, including
// cleanup. Closing admission does not close a channel that producers send to.
type sinkQueue struct {
	mu      sync.Mutex
	items   [sinkQueueRecords]queuedLog
	head    int
	stats   SinkStats
	changed chan struct{}
	done    chan struct{}
	write   func([]byte) (int, error)
	cleanup func() error
	writeAt func([]byte, time.Time) (int, error)
}

type queuedLog struct {
	data []byte
	at   time.Time
}

func newSinkQueue(write func([]byte) (int, error), cleanup func() error) *sinkQueue {
	return newTimedSinkQueue(write, nil, cleanup)
}

func newTimedSinkQueue(write func([]byte) (int, error), writeAt func([]byte, time.Time) (int, error), cleanup func() error) *sinkQueue {
	q := &sinkQueue{changed: make(chan struct{}), done: make(chan struct{}), write: write, writeAt: writeAt, cleanup: cleanup}
	go q.run()
	return q
}

func (q *sinkQueue) signalLocked() { close(q.changed); q.changed = make(chan struct{}) }

func (q *sinkQueue) enqueue(p []byte) {
	q.enqueueAt(p, time.Time{})
}

func (q *sinkQueue) enqueueAt(p []byte, at time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stats.Closed || q.stats.Queued == len(q.items) || len(p) > sinkQueueBytes-q.stats.QueuedBytes {
		q.stats.Dropped++
		q.stats.DroppedByLevel.add(p)
		q.stats.DroppedBytes += uint64(len(p))
		if q.stats.Closed {
			q.stats.RejectedAfterClose++
		}
		return
	}
	q.items[(q.head+q.stats.Queued)%len(q.items)] = queuedLog{data: append([]byte(nil), p...), at: at}
	q.stats.Queued++
	q.stats.QueuedBytes += len(p)
	q.stats.Accepted++
	q.signalLocked()
}

func (q *sinkQueue) run() {
	defer close(q.done)
	for {
		q.mu.Lock()
		if q.stats.Queued == 0 {
			closed, ch := q.stats.Closed, q.changed
			q.mu.Unlock()
			if closed {
				break
			}
			<-ch
			continue
		}
		record := q.items[q.head]
		p := record.data
		q.items[q.head] = queuedLog{}
		q.head = (q.head + 1) % len(q.items)
		q.stats.Queued--
		q.stats.QueuedBytes -= len(p)
		q.stats.InFlight = true
		q.mu.Unlock()
		var n int
		var err error
		if q.writeAt != nil {
			n, err = q.writeAt(p, record.at)
		} else {
			n, err = q.write(p)
		}
		q.mu.Lock()
		if err != nil || n != len(p) {
			q.stats.Errors++
		}
		q.stats.Completed++
		q.stats.InFlight = false
		q.signalLocked()
		q.mu.Unlock()
	}
	if q.cleanup != nil {
		if err := q.cleanup(); err != nil {
			q.mu.Lock()
			q.stats.Errors++
			q.mu.Unlock()
		}
	}
}

func (q *sinkQueue) snapshot() SinkStats { q.mu.Lock(); defer q.mu.Unlock(); return q.stats }

func (q *sinkQueue) stop() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.stats.Closed {
		q.stats.Closed = true
		q.signalLocked()
	}
}

// flush waits only for records admitted before this call. Failed writes count
// as completed attempts; inspect Errors to distinguish delivery from attempts.
func (q *sinkQueue) flush(ctx context.Context) error {
	q.mu.Lock()
	target := q.stats.Accepted
	for q.stats.Completed < target {
		ch := q.changed
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
		q.mu.Lock()
	}
	q.mu.Unlock()
	return nil
}

type AsyncStats struct {
	File          SinkStats    `json:"file"`
	Console       SinkStats    `json:"console"`
	Truncated     uint64       `json:"truncated"`
	Storage       StorageStats `json:"storage"`
	CloseTimeouts uint64       `json:"close_timeouts"`
}

// AsyncFiles isolates the existing rooted file store from logging producers.
// Each sink has its own budget, so a blocked console cannot starve file output.
// Rotation/quota remains the responsibility of Files.
type AsyncFiles struct {
	files         *Files
	file, console *sinkQueue
	truncated     atomic.Uint64
	now           func() time.Time
	closeTimeouts atomic.Uint64
}

func NewAsyncFiles(opts FileOptions) (*AsyncFiles, error) {
	console := opts.Console
	if console == nil {
		console = os.Stdout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	a := &AsyncFiles{now: opts.Now}
	a.console = newSinkQueue(console.Write, nil) // stdout is borrowed, never closed.
	opts.Console = io.Discard
	opts.Errors = asyncDiagnosticWriter{a}
	f, err := NewFiles(opts)
	if err != nil {
		a.console.stop()
		return nil, err
	}
	a.files = f
	a.file = newTimedSinkQueue(nil, f.writeAt, f.Close)
	return a, nil
}

type asyncDiagnosticWriter struct{ owner *AsyncFiles }

func (w asyncDiagnosticWriter) Write(p []byte) (int, error) {
	w.owner.console.enqueue(w.owner.prepare(p))
	return len(p), nil
}

func (a *AsyncFiles) prepare(p []byte) []byte {
	p = []byte(SanitizeText(string(p)))
	if len(p) <= maxLogRecordBytes {
		return p
	}
	const marker = "… [truncated]\n"
	end := maxLogRecordBytes - len(marker)
	for end > 0 && !utf8.RuneStart(p[end]) {
		end--
	}
	a.truncated.Add(1)
	return append(p[:end:end], []byte(marker)...)
}

func (a *AsyncFiles) Write(p []byte) (int, error) {
	at := a.now()
	data := a.prepare(p)
	a.console.enqueue(data)
	a.file.enqueueAt(data, at)
	return len(p), nil
}

func (a *AsyncFiles) Stats() AsyncStats {
	return AsyncStats{File: a.file.snapshot(), Console: a.console.snapshot(), Truncated: a.truncated.Load(), Storage: a.files.StorageStats(), CloseTimeouts: a.closeTimeouts.Load()}
}
func (a *AsyncFiles) OpenLogs(ctx context.Context, s Selection) (io.ReadCloser, error) {
	return a.files.OpenLogs(ctx, s)
}
func (a *AsyncFiles) Flush(ctx context.Context) error {
	if err := a.file.flush(ctx); err != nil {
		return err
	}
	return a.console.flush(ctx)
}

// CloseContext stops admission before waiting. A timed-out syscall still owns
// its resource; its worker drains and closes it when the syscall returns.
func (a *AsyncFiles) CloseContext(ctx context.Context) error {
	a.file.stop()
	a.console.stop()
	for _, q := range []*sinkQueue{a.file, a.console} {
		select {
		case <-q.done:
		case <-ctx.Done():
			a.closeTimeouts.Add(1)
			return ctx.Err()
		}
	}
	return nil
}
func (a *AsyncFiles) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), logDrainTimeout)
	defer cancel()
	return a.CloseContext(ctx)
}
