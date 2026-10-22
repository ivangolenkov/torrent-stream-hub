package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/repository"
	"torrent-stream-hub/internal/usecase"
)

type sseTestWriter struct {
	*httptest.ResponseRecorder
	writeErr, flushErr error
	deadline           time.Time
	writes             int
	block              bool
}

func (w *sseTestWriter) SetWriteDeadline(deadline time.Time) error { w.deadline = deadline; return nil }
func (w *sseTestWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.block {
		<-time.After(time.Until(w.deadline))
		return 0, os.ErrDeadlineExceeded
	}
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.ResponseRecorder.Write(p)
}
func (w *sseTestWriter) FlushError() error { return w.flushErr }

func (w *sseTestWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func TestSSELifecycleAndWriteErrors(t *testing.T) {
	dir := t.TempDir()
	db, err := repository.NewSQLiteDB(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e, err := engine.New(&config.Config{DownloadDir: dir, BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for _, mode := range []string{"write", "flush", "blocked", "shutdown", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			sw := usecase.NewSyncWorker(e, repository.NewTorrentRepo(db))
			defer sw.Close()
			h := NewAPIHandler(nil, sw)
			h.sseWriteTimeout = 20 * time.Millisecond
			writer := &sseTestWriter{ResponseRecorder: httptest.NewRecorder()}
			if mode == "blocked" {
				writer.block = true
				sw.Refresh()
			}
			if mode == "write" {
				writer.writeErr = errors.New("write failed")
				sw.Refresh()
			}
			if mode == "flush" {
				writer.flushErr = errors.New("flush failed")
				sw.Refresh()
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				if mode == "unsupported" {
					h.SSEEvents(writer.ResponseRecorder, httptest.NewRequest(http.MethodGet, "/events", nil))
				} else {
					h.SSEEvents(writer, httptest.NewRequest(http.MethodGet, "/events", nil))
				}
			}()
			if mode == "shutdown" {
				sw.Close()
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("SSE handler leaked")
			}
			if mode == "unsupported" && writer.Code != http.StatusServiceUnavailable {
				t.Fatal("unsupported writer accepted")
			}
			if (mode == "write" || mode == "flush" || mode == "blocked") && writer.writes != 1 {
				t.Fatal("write failure ignored")
			}
		})
	}
}
