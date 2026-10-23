package api

import (
	"context"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"torrent-stream-hub/internal/usecase"
)

type snapshotFrameWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *snapshotFrameWriter) SetWriteDeadline(time.Time) error { return nil }
func (w *snapshotFrameWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	w.cancel()
	return n, err
}
func (w *snapshotFrameWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func TestSnapshotVersionEmptyRESTSSEAndCommandReceipt(t *testing.T) {
	uc, _, _, cleanup := setupAPITestUseCase(t)
	defer cleanup()
	sw := usecase.NewSyncWorker(uc)
	defer sw.Close()
	h := NewAPIHandler(uc, sw)
	router := chi.NewRouter()
	h.RegisterRoutes(router)
	list := httptest.NewRecorder()
	router.ServeHTTP(list, httptest.NewRequest("GET", "/torrents", nil))
	if list.Code != 200 || strings.TrimSpace(list.Body.String()) != "[]" {
		t.Fatal(list.Code, list.Body.String())
	}
	first := strings.Split(list.Header().Get("X-Hub-Snapshot"), "/")
	if len(first) != 2 {
		t.Fatal("empty list lacks an epoch/sequence", list.Header())
	}
	sw.Refresh()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &snapshotFrameWriter{httptest.NewRecorder(), cancel}
	h.SSEEvents(writer, httptest.NewRequest("GET", "/events", nil).WithContext(ctx))
	frame := strings.Split(strings.TrimPrefix(writer.Body.String(), "id: "), "\n")
	if len(frame) < 2 || frame[1] != "data: []" {
		t.Fatal("SSE array compatibility changed", writer.Body.String())
	}
	publication := strings.Split(frame[0], "/")
	tor, err := uc.AddMagnet("magnet:?xt=urn:btih:0123456789012345678901234567890123456789")
	if err != nil {
		t.Fatal(err)
	}
	ack := httptest.NewRecorder()
	router.ServeHTTP(ack, httptest.NewRequest("POST", "/torrent/"+tor.Hash+"/action", strings.NewReader(`{"action":"pause"}`)))
	if ack.Code != 200 {
		t.Fatal(ack.Code, ack.Body.String())
	}
	receipt := strings.Split(ack.Header().Get("X-Hub-Snapshot"), "/")
	if len(publication) != 2 || len(receipt) != 2 || first[0] != publication[0] || publication[0] != receipt[0] {
		t.Fatal(first, publication, receipt)
	}
	a, _ := strconv.ParseUint(first[1], 10, 64)
	b, _ := strconv.ParseUint(publication[1], 10, 64)
	c, _ := strconv.ParseUint(receipt[1], 10, 64)
	if !(a < b && b < c) {
		t.Fatal("capture/ack order not monotonic", first, publication, receipt)
	}
}
