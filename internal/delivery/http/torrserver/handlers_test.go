package torrserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"torrent-stream-hub/internal/models"
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

	var body struct {
		CacheSize int64 `json:"CacheSize"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if body.CacheSize == 0 {
		t.Fatalf("expected CacheSize for Lampa TorrServer compatibility")
	}
}

func TestTorrentResponseUsesTorrServerCompatibleFields(t *testing.T) {
	torrent := &models.Torrent{
		Hash: "abc123",
		Name: "[LAMPA] Movie",
		Size: 2048,
		Files: []*models.File{
			{Index: 3, Path: "Movie/Movie.mkv", Size: 2048},
		},
	}

	body := toTorrentResponse(torrent, true)

	if body.Hash != torrent.Hash {
		t.Fatalf("expected hash %q, got %q", torrent.Hash, body.Hash)
	}
	if body.Title != torrent.Name {
		t.Fatalf("expected title %q, got %q", torrent.Name, body.Title)
	}
	if body.Data != "{}" {
		t.Fatalf("expected JSON object string data, got %q", body.Data)
	}
	if len(body.FileStats) != 1 {
		t.Fatalf("expected one file_stat, got %d", len(body.FileStats))
	}
	if body.FileStats[0].ID != 3 || body.FileStats[0].Path != "Movie/Movie.mkv" || body.FileStats[0].Length != 2048 {
		t.Fatalf("unexpected file_stat: %+v", body.FileStats[0])
	}
}

func TestStreamContentTypeAvoidsBlockingSniff(t *testing.T) {
	if got := streamContentType("The.Sopranos.S01E01.avi"); got != "video/x-msvideo" {
		t.Fatalf("expected AVI content type, got %q", got)
	}
	if got := streamContentType("file.unknownext"); got != "application/octet-stream" {
		t.Fatalf("expected fallback content type, got %q", got)
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
