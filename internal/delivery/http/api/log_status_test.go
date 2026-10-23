package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"torrent-stream-hub/internal/logging"
)

type statusOnlyLogs struct{}

func (statusOnlyLogs) OpenLogs(context.Context, logging.Selection) (io.ReadCloser, error) {
	panic("stats must not open logs")
}
func (statusOnlyLogs) Stats() logging.AsyncStats {
	return logging.AsyncStats{Storage: logging.StorageStats{QuotaPaused: true, LostRecords: 7}, Console: logging.SinkStats{InFlight: true, Dropped: 9}}
}

func TestLogsStatusDoesNotReadSink(t *testing.T) {
	rr := httptest.NewRecorder()
	LogsStatusHandler(statusOnlyLogs{})(rr, httptest.NewRequest(http.MethodGet, "/logs/status", nil))
	if rr.Code != 200 || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(rr.Code, rr.Header())
	}
	var stats logging.AsyncStats
	if err := json.Unmarshal(rr.Body.Bytes(), &stats); err != nil || !stats.Storage.QuotaPaused || stats.Console.Dropped != 9 {
		t.Fatal(stats, err)
	}
	rr = httptest.NewRecorder()
	LogsStatusHandler(nil)(rr, httptest.NewRequest(http.MethodGet, "/logs/status", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatal(rr.Code)
	}
}
