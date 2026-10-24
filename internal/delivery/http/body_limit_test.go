package http

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCacheOversizeKeepsCORS(t *testing.T) {
	router := NewRouter(nil, nil)
	r := httptest.NewRequest("POST", "/cache", strings.NewReader(strings.Repeat("x", 1<<20+1)))
	r.ContentLength = -1
	r.Header.Set("Origin", "https://player.example")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != 413 || w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal(w.Code, w.Header(), w.Body.String())
	}
}
