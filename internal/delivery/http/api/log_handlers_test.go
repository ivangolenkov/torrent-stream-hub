package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"torrent-stream-hub/internal/logging"
)

func TestParseLogSelection(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.Local)
	for _, raw := range []string{"", "date=2026-10-19", "from=2026-10-18&to=2026-10-20&tail=500", "tail=100000"} {
		query, _ := url.ParseQuery(raw)
		selection, err := parseLogSelection(query, now)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if raw == "" && (selection.From != "2026-10-20" || selection.To != selection.From) {
			t.Fatal(selection)
		}
	}
	for _, raw := range []string{"date=", "date=2026-02-30", "date=2026-1-01", "date=../secret", "date=2026-10-19&from=2026-10-19&to=2026-10-20", "from=2026-10-19", "to=2026-10-19", "from=2026-10-20&to=2026-10-19", "date=2026-10-20&date=2026-10-20", "tail=1&tail=2", "tail=", "tail=0", "tail=-1", "tail=%2B1", "tail=100001", "tail=999999999999999999999999999999", "tail=1.5", "from=&to="} {
		query, _ := url.ParseQuery(raw)
		if _, err := parseLogSelection(query, now); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestLogsHandlerFiles(t *testing.T) {
	dir := t.TempDir()
	f, err := logging.NewFiles(logging.FileOptions{Dir: dir, RetentionDays: 36500, Console: io.Discard, Errors: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for date, text := range map[string]string{"2026-10-19": "old\n", "2026-10-20": "new\nlast\n", "2026-10-18": ""} {
		if err := os.WriteFile(filepath.Join(dir, "hub-"+date+".log"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		query  string
		status int
		body   string
	}{
		{"date=2026-10-20", 200, "new\nlast\n"},
		{"date=2026-10-20&tail=1", 200, "last\n"},
		{"from=2026-10-18&to=2026-10-20&tail=2", 200, "new\nlast\n"},
		{"from=2026-10-18&to=2026-10-20", 200, "old\nnew\nlast\n"},
		{"date=2026-10-18", 200, ""},
		{"date=2000-01-01", 404, ""},
		{"tail=0", 400, ""},
		{"date=%zz", 400, ""},
	} {
		rr := httptest.NewRecorder()
		LogsHandler(f).ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/logs?"+tc.query, nil))
		if rr.Code != tc.status || rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d %s", tc.query, rr.Code, rr.Body)
		}
		if tc.status == 200 && (rr.Body.String() != tc.body || rr.Header().Get("Content-Type") != "text/plain; charset=utf-8") {
			t.Fatalf("%s: %q", tc.query, rr.Body.String())
		}
	}
}

type errorLogReader struct{ afterHeader bool }

func (e errorLogReader) OpenLogs(context.Context, logging.Selection) (io.ReadCloser, error) {
	if e.afterHeader {
		return io.NopCloser(brokenLogStream{}), nil
	}
	return nil, errors.New("private /internal/secret/path")
}

type brokenLogStream struct{}

func (brokenLogStream) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestLogHandlerErrorsDoNotExposePathsOrAppendJSON(t *testing.T) {
	for _, afterHeader := range []bool{false, true} {
		rr := httptest.NewRecorder()
		LogsHandler(errorLogReader{afterHeader: afterHeader}).ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/logs", nil))
		if strings.Contains(rr.Body.String(), "/internal") {
			t.Fatal(rr.Body)
		}
		if afterHeader && (rr.Code != 200 || rr.Body.Len() != 0) {
			t.Fatalf("%d: %s", rr.Code, rr.Body)
		}
		if !afterHeader && rr.Code != http.StatusInternalServerError {
			t.Fatal(rr.Code)
		}
	}
}
