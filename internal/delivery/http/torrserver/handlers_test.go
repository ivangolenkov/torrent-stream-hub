package torrserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
	"torrent-stream-hub/internal/usecase"
)

const torrserverTestInfoHash = "0123456789abcdef0123456789abcdef01234567"

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

	req, _ := http.NewRequest("POST", "/settings", strings.NewReader(`{"action":"get"}`))
	rr := httptest.NewRecorder()

	http.HandlerFunc(h.Settings).ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	var body struct {
		CacheSize       int64 `json:"CacheSize"`
		ReaderReadAHead int64 `json:"ReaderReadAHead"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if body.CacheSize == 0 {
		t.Fatalf("expected CacheSize for Lampa TorrServer compatibility")
	}
	if body.ReaderReadAHead == 0 {
		t.Fatalf("expected ReaderReadAHead for TorrServer compatibility")
	}
}

func TestSettingsSetAndDefAreNoOpCompatibilityResponses(t *testing.T) {
	h := NewTorrServerHandler(nil)
	for _, action := range []string{"set", "def"} {
		req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(`{"action":"`+action+`"}`))
		rr := httptest.NewRecorder()

		h.Settings(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected status %d for action %s, got %d", http.StatusOK, action, rr.Code)
		}
	}
}

func TestTorrentResponseUsesTorrServerCompatibleFields(t *testing.T) {
	torrent := &models.Torrent{
		Hash:     "abc123",
		Name:     "[LAMPA] Movie",
		Title:    "LAMPA Title",
		Data:     `{"id":"1"}`,
		Poster:   "https://example.com/poster.jpg",
		Category: "movies",
		Size:     2048,
		Files: []*models.File{
			{Index: 3, Path: "Movie/Movie.mkv", Size: 2048},
		},
	}

	body := toTorrentResponse(torrent, true)

	if body.Hash != torrent.Hash {
		t.Fatalf("expected hash %q, got %q", torrent.Hash, body.Hash)
	}
	if body.Title != torrent.Title {
		t.Fatalf("expected title %q, got %q", torrent.Title, body.Title)
	}
	if body.Data != torrent.Data || body.Poster != torrent.Poster || body.Category != torrent.Category {
		t.Fatalf("unexpected metadata response: %+v", body)
	}
	if len(body.FileStats) != 1 {
		t.Fatalf("expected one file_stat, got %d", len(body.FileStats))
	}
	if body.FileStats[0].ID != 4 || body.FileStats[0].Path != "Movie/Movie.mkv" || body.FileStats[0].Length != 2048 {
		t.Fatalf("unexpected file_stat: %+v", body.FileStats[0])
	}
}

func TestTorrServerFileIDMapping(t *testing.T) {
	if got := internalIndexToTorrserver(0); got != 1 {
		t.Fatalf("expected internal 0 to become TorrServer id 1, got %d", got)
	}
	if got := torrserverIndexToInternal(1); got != 0 {
		t.Fatalf("expected TorrServer id 1 to become internal 0, got %d", got)
	}
	if got := torrserverIndexToInternal(0); got != 0 {
		t.Fatalf("expected id 0 fallback to stay internal 0, got %d", got)
	}
}

func TestCacheRequestAcceptsJSONWithFormContentType(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/cache", strings.NewReader(`{"action":"get","hash":"abc","index":1}`))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")

	body, err := decodeCacheRequest(req)
	if err != nil {
		t.Fatalf("failed to decode cache request: %v", err)
	}
	if body.Action != "get" || body.Hash != "abc" || body.Index != 1 {
		t.Fatalf("unexpected cache request: %+v", body)
	}
}

func TestResolveHTTPLinkSupportsMagnetRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", http.StatusFound)
	}))
	defer server.Close()

	link, data, err := resolveHTTPLink(server.URL)
	if err != nil {
		t.Fatalf("expected magnet redirect to resolve, got error: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("expected no torrent data for magnet redirect, got %d bytes", len(data))
	}
	if link != "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("unexpected resolved link: %q", link)
	}
}

func TestTorrentsAddPersistsEvenWhenSaveToDBFalse(t *testing.T) {
	_, repo, h, cleanup := setupTorrServerIntegration(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/torrents", strings.NewReader(`{
		"action":"add",
		"link":"magnet:?xt=urn:btih:`+torrserverTestInfoHash+`",
		"title":"[LAMPA] Test",
		"poster":"https://example.com/poster.jpg",
		"data":"{}",
		"save_to_db":false
	}`))
	rr := httptest.NewRecorder()

	h.Torrents(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rr.Code, rr.Body.String())
	}
	fetched, err := repo.GetTorrent(torrserverTestInfoHash)
	if err != nil {
		t.Fatalf("failed to get persisted torrent: %v", err)
	}
	if fetched == nil {
		t.Fatalf("expected torrent to be persisted despite save_to_db=false")
	}
	if fetched.Title != "[LAMPA] Test" || fetched.Poster != "https://example.com/poster.jpg" {
		t.Fatalf("expected metadata to be persisted, got %+v", fetched)
	}
}

func TestTorrentsDropAndWipeAreSafeNoOps(t *testing.T) {
	_, repo, h, cleanup := setupTorrServerIntegration(t)
	defer cleanup()

	if _, err := h.uc.AddMagnet("magnet:?xt=urn:btih:" + torrserverTestInfoHash); err != nil {
		t.Fatalf("failed to add torrent: %v", err)
	}

	for _, action := range []string{"drop", "wipe"} {
		req := httptest.NewRequest(http.MethodPost, "/torrents", strings.NewReader(`{"action":"`+action+`","hash":"`+torrserverTestInfoHash+`"}`))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
		rr := httptest.NewRecorder()

		h.Torrents(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected %s status %d, got %d: %s", action, http.StatusOK, rr.Code, rr.Body.String())
		}
		fetched, err := repo.GetTorrent(torrserverTestInfoHash)
		if err != nil {
			t.Fatalf("failed to get torrent after %s: %v", action, err)
		}
		if fetched == nil {
			t.Fatalf("expected torrent to remain after %s", action)
		}
	}
}

func TestTorrentsRemDeletesFromDB(t *testing.T) {
	_, repo, h, cleanup := setupTorrServerIntegration(t)
	defer cleanup()

	if _, err := h.uc.AddMagnet("magnet:?xt=urn:btih:" + torrserverTestInfoHash); err != nil {
		t.Fatalf("failed to add torrent: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/torrents", strings.NewReader(`{"action":"rem","hash":"`+torrserverTestInfoHash+`"}`))
	rr := httptest.NewRecorder()

	h.Torrents(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rr.Code, rr.Body.String())
	}
	fetched, err := repo.GetTorrent(torrserverTestInfoHash)
	if err != nil {
		t.Fatalf("failed to get torrent after rem: %v", err)
	}
	if fetched != nil {
		t.Fatalf("expected torrent to be deleted by rem, got %+v", fetched)
	}
}

func setupTorrServerIntegration(t *testing.T) (string, *repository.TorrentRepo, *TorrServerHandler, func()) {
	t.Helper()
	dir := t.TempDir()
	db, err := repository.NewSQLiteDB(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatalf("failed to create test DB: %v", err)
	}

	eng, err := engine.New(&config.Config{
		DownloadDir:        dir,
		TorrentPort:        0,
		MaxActiveDownloads: 1,
		MinFreeSpaceGB:     0,
	})
	if err != nil {
		db.Close()
		t.Fatalf("failed to create engine: %v", err)
	}

	repo := repository.NewTorrentRepo(db)
	h := NewTorrServerHandler(usecase.NewTorrentUseCase(eng, repo))
	cleanup := func() {
		eng.Close()
		db.Close()
	}
	return dir, repo, h, cleanup
}

func TestStreamContentTypeAvoidsBlockingSniff(t *testing.T) {
	if got := streamContentType("The.Sopranos.S01E01.avi"); got != "video/x-msvideo" {
		t.Fatalf("expected AVI content type, got %q", got)
	}
	if got := streamContentType("movie.mkv"); got != "video/x-matroska" {
		t.Fatalf("expected MKV content type, got %q", got)
	}
	if got := streamContentType("file.unknownext"); got != "application/octet-stream" {
		t.Fatalf("expected fallback content type, got %q", got)
	}
}

func TestPreloadTargetPersistsForStatPolling(t *testing.T) {
	h := NewTorrServerHandler(nil)
	h.setPreload("hash", 0, preloadState{TargetBytes: 12345, StartedAt: time.Now()})

	if got := h.preloadTarget("hash", 0, 999999); got != 12345 {
		t.Fatalf("expected persisted preload target, got %d", got)
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
