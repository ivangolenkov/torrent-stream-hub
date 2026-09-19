package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
