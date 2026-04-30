package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAddTorrentInvalidJSONReturnsJSONError(t *testing.T) {
	h := NewAPIHandler(nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/torrent/add", strings.NewReader("{"))
	rr := httptest.NewRecorder()

	h.AddTorrent(rr, req)

	assertJSONError(t, rr, http.StatusBadRequest, "Invalid JSON")
}

func TestTorrentActionUnknownActionReturnsJSONError(t *testing.T) {
	h := NewAPIHandler(nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/torrent/hash/action", strings.NewReader(`{"action":"bad"}`))
	rr := httptest.NewRecorder()

	h.TorrentAction(rr, req)

	assertJSONError(t, rr, http.StatusBadRequest, "Unknown action")
}

func assertJSONError(t *testing.T, rr *httptest.ResponseRecorder, status int, message string) {
	t.Helper()

	if rr.Code != status {
		t.Fatalf("expected status %d, got %d", status, rr.Code)
	}
	if contentType := rr.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("expected JSON content type, got %q", contentType)
	}

	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if body.Error != message {
		t.Fatalf("expected error %q, got %q", message, body.Error)
	}
}
