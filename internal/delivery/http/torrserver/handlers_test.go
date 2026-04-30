package torrserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEchoHandler(t *testing.T) {
	h := NewTorrServerHandler(nil) // nil usecase is fine for Echo

	req, err := http.NewRequest("GET", "/echo", nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(h.Echo)

	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, http.StatusOK)
	}

	expected := `1.2.133`
	if rr.Body.String() != expected {
		t.Errorf("handler returned unexpected body: got %v want %v",
			rr.Body.String(), expected)
	}
}

func TestSettingsHandler(t *testing.T) {
	h := NewTorrServerHandler(nil)

	req, _ := http.NewRequest("POST", "/settings", nil)
	rr := httptest.NewRecorder()

	http.HandlerFunc(h.Settings).ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	expected := `{}`
	if rr.Body.String() != expected {
		t.Errorf("Expected body %s, got %s", expected, rr.Body.String())
	}
}

func TestTorrentsInvalidJSONReturnsJSONError(t *testing.T) {
	h := NewTorrServerHandler(nil)

	req := httptest.NewRequest(http.MethodPost, "/torrents", strings.NewReader("{"))
	rr := httptest.NewRecorder()

	h.Torrents(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rr.Code)
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
	if body.Error != "Invalid JSON" {
		t.Fatalf("expected Invalid JSON error, got %q", body.Error)
	}
}
