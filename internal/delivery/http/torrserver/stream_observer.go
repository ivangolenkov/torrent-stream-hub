package torrserver

import (
	"bufio"
	"net"
	"net/http"
	"sync"
	"time"
)

type streamResponseWriter struct {
	http.ResponseWriter
	statusCode     int
	bytesWritten   int64
	firstWriteTime time.Time
	headerWritten  bool
	mu             sync.Mutex
	startTime      time.Time
	prepDuration   time.Duration
}

func newStreamResponseWriter(w http.ResponseWriter, startTime time.Time, prepDuration time.Duration) *streamResponseWriter {
	return &streamResponseWriter{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
		startTime:      startTime,
		prepDuration:   prepDuration,
	}
}

func (w *streamResponseWriter) WriteHeader(code int) {
	w.mu.Lock()
	if !w.headerWritten {
		w.statusCode = code
		w.headerWritten = true
	}
	w.mu.Unlock()
	w.ResponseWriter.WriteHeader(code)
}

func (w *streamResponseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if !w.headerWritten {
		w.headerWritten = true
	}
	if w.firstWriteTime.IsZero() && len(p) > 0 {
		w.firstWriteTime = time.Now()
	}
	w.mu.Unlock()

	n, err := w.ResponseWriter.Write(p)
	w.mu.Lock()
	w.bytesWritten += int64(n)
	w.mu.Unlock()
	return n, err
}

func (w *streamResponseWriter) TimeToFirstWrite() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.firstWriteTime.IsZero() {
		return 0
	}
	return w.firstWriteTime.Sub(w.startTime)
}

func (w *streamResponseWriter) StatusCode() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.statusCode
}

func (w *streamResponseWriter) BytesWritten() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bytesWritten
}

func (w *streamResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *streamResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hijacker.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}
