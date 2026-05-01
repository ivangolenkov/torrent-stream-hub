package http

import (
	"net/http"
	"net/http/httptest"
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
