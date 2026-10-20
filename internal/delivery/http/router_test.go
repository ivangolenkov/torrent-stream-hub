package http

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"torrent-stream-hub/internal/logging"
)

func TestLogsRouteAndCORS(t *testing.T) {
	files, err := logging.NewFiles(logging.FileOptions{Dir: t.TempDir(), Console: io.Discard, Errors: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	_, _ = files.Write([]byte("example\n"))
	router := NewRouterWithLogs(nil, nil, files)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs?tail=1", nil)
	req.Header.Set("Origin", "http://lampa.app")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != 200 || rr.Body.String() != "example\n" || rr.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("%d %q %v", rr.Code, rr.Body.String(), rr.Header())
	}
}

func TestAccessAndPanicLogsShareFilesWithoutQuerySecrets(t *testing.T) {
	files, err := logging.NewFiles(logging.FileOptions{Dir: t.TempDir(), Console: io.Discard, Errors: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	restore := logging.UseFiles(files)
	defer restore()
	oldLevel := logging.CurrentLevel()
	logging.Configure("info")
	defer logging.Configure(oldLevel)
	handler := accessLogger(jsonRecoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("test panic token=private") })))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test?token=secret", nil)
	handler.ServeHTTP(rr, req)
	r, err := files.OpenLogs(context.Background(), logging.Selection{From: "2000-01-01", To: "9999-12-31"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	p, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	text := string(p)
	if rr.Code != 500 || !strings.Contains(text, "status=500") || !strings.Contains(text, "runtime/debug.Stack") || strings.Contains(text, "private") || strings.Contains(text, "secret") || strings.Contains(text, "\x1b") {
		t.Fatalf("status=%d logs=%s", rr.Code, text)
	}
}

func TestLogDownloadDoesNotIncludeItsOwnAccessLog(t *testing.T) {
	files, err := logging.NewFiles(logging.FileOptions{Dir: t.TempDir(), Console: io.Discard, Errors: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	restore := logging.UseFiles(files)
	defer restore()
	_, _ = files.Write([]byte("before request\n"))
	router := NewRouterWithLogs(nil, nil, files)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/logs", nil))
	if rr.Code != 200 || rr.Body.String() != "before request\n" {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
}

func TestCORSAllowsWildcardOriginWithoutCredentials(t *testing.T) {
	router := NewRouter(nil, nil)
	req := httptest.NewRequest(http.MethodOptions, "/torrents", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("expected wildcard CORS origin, got %q", rr.Header().Get("Access-Control-Allow-Origin"))
	}
	if rr.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("expected no credentialed wildcard CORS header, got %q", rr.Header().Get("Access-Control-Allow-Credentials"))
	}
}

func TestCORSAllowsHEADAndRangePreflight(t *testing.T) {
	router := NewRouter(nil, nil)

	// Preflight OPTIONS for HEAD with Range header
	req := httptest.NewRequest(http.MethodOptions, "/stream/hash/0", nil)
	req.Header.Set("Origin", "http://lampa.app")
	req.Header.Set("Access-Control-Request-Method", http.MethodHead)
	req.Header.Set("Access-Control-Request-Headers", "Range")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("expected wildcard CORS origin, got %q", rr.Header().Get("Access-Control-Allow-Origin"))
	}
	allowedMethods := rr.Header().Get("Access-Control-Allow-Methods")
	if !strings.Contains(allowedMethods, "HEAD") {
		t.Fatalf("expected HEAD in allowed methods, got %q", allowedMethods)
	}

	// Preflight OPTIONS for GET
	reqGet := httptest.NewRequest(http.MethodOptions, "/stream/hash/0", nil)
	reqGet.Header.Set("Origin", "http://lampa.app")
	reqGet.Header.Set("Access-Control-Request-Method", http.MethodGet)
	rrGet := httptest.NewRecorder()
	router.ServeHTTP(rrGet, reqGet)
	if !strings.Contains(rrGet.Header().Get("Access-Control-Allow-Methods"), "GET") {
		t.Fatalf("expected GET in allowed methods, got %q", rrGet.Header().Get("Access-Control-Allow-Methods"))
	}

	// Direct GET request has wildcard origin and exposed headers
	getReq := httptest.NewRequest(http.MethodGet, "/echo", nil)
	getReq.Header.Set("Origin", "http://lampa.app")
	getRr := httptest.NewRecorder()
	router.ServeHTTP(getRr, getReq)

	if getRr.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("expected wildcard CORS origin on GET, got %q", getRr.Header().Get("Access-Control-Allow-Origin"))
	}
	exposedHeaders := getRr.Header().Get("Access-Control-Expose-Headers")
	if !strings.Contains(exposedHeaders, "Content-Range") {
		t.Fatalf("expected Content-Range in exposed headers, got %q", exposedHeaders)
	}
	if !strings.Contains(exposedHeaders, "Content-Length") {
		t.Fatalf("expected Content-Length in exposed headers, got %q", exposedHeaders)
	}
	if !strings.Contains(exposedHeaders, "Accept-Ranges") {
		t.Fatalf("expected Accept-Ranges in exposed headers, got %q", exposedHeaders)
	}
}
