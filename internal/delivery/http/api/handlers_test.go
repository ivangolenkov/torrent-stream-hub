package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/repository"
	"torrent-stream-hub/internal/usecase"
)

const apiTestInfoHash = "0123456789abcdef0123456789abcdef01234567"

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

func TestGetAllTorrentsIncludesPeerSummary(t *testing.T) {
	dir := t.TempDir()
	db, err := repository.NewSQLiteDB(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatalf("failed to create test DB: %v", err)
	}
	defer db.Close()

	eng, err := engine.New(&config.Config{
		DownloadDir:        dir,
		TorrentPort:        0,
		MaxActiveDownloads: 1,
		MinFreeSpaceGB:     0,
	})
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer eng.Close()

	repo := repository.NewTorrentRepo(db)
	uc := usecase.NewTorrentUseCase(eng, repo)
	if _, err := uc.AddMagnet("magnet:?xt=urn:btih:" + apiTestInfoHash); err != nil {
		t.Fatalf("failed to add test torrent: %v", err)
	}

	h := NewAPIHandler(uc, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/torrents", nil)
	rr := httptest.NewRecorder()

	h.GetAllTorrents(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rr.Code)
	}

	var body []map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if len(body) != 1 {
		t.Fatalf("expected one torrent, got %d", len(body))
	}
	if _, ok := body[0]["peer_summary"]; !ok {
		t.Fatalf("expected peer_summary field in response: %#v", body[0])
	}
	if _, ok := body[0]["download_speed"]; !ok {
		t.Fatalf("expected download_speed field in response: %#v", body[0])
	}
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
