package torrserver

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
	"torrent-stream-hub/internal/usecase"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
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

func createTorrserverTestTorrent(pieceLength int64, name string, files []metainfo.FileInfo, fileData [][]byte) []byte {
	var allData []byte
	for _, d := range fileData {
		allData = append(allData, d...)
	}
	numPieces := int((int64(len(allData)) + pieceLength - 1) / pieceLength)
	if numPieces == 0 {
		numPieces = 1
	}

	pieces := make([]byte, 0, numPieces*20)
	for i := 0; i < numPieces; i++ {
		start := int64(i) * pieceLength
		end := start + pieceLength
		if end > int64(len(allData)) {
			end = int64(len(allData))
		}
		var chunk []byte
		if start < int64(len(allData)) {
			chunk = allData[start:end]
		}
		h := sha1.Sum(chunk)
		pieces = append(pieces, h[:]...)
	}

	info := metainfo.Info{
		Name:        name,
		PieceLength: pieceLength,
		Pieces:      pieces,
		Files:       files,
	}
	if len(files) == 1 && len(files[0].Path) == 0 {
		info.Length = files[0].Length
		info.Files = nil
	}

	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		panic(err)
	}
	mi := metainfo.MetaInfo{
		InfoBytes: infoBytes,
	}
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func TestPreload_CompletedFileReturnsFastPath(t *testing.T) {
	dir, _, h, cleanup := setupTorrServerIntegration(t)
	defer cleanup()

	fileData := [][]byte{bytes.Repeat([]byte("F"), 4096)}
	torrentBytes := createTorrserverTestTorrent(1024, "test_preload_fast", []metainfo.FileInfo{
		{Length: 4096, Path: []string{"video.mp4"}},
	}, fileData)

	tor, err := h.uc.AddTorrentFile(bytes.NewReader(torrentBytes))
	if err != nil {
		t.Fatalf("failed to add torrent: %v", err)
	}
	hash := tor.Hash

	filePath := filepath.Join(dir, "test_preload_fast", "video.mp4")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	if err := os.WriteFile(filePath, fileData[0], 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	// Verify pieces in engine
	tf, err := h.uc.GetTorrentFile(hash, 0)
	if err != nil {
		t.Fatalf("failed to get torrent file: %v", err)
	}
	tRaw := tf.Torrent()
	for i := 0; i < tRaw.NumPieces(); i++ {
		_ = tRaw.Piece(i).VerifyData()
	}
	deadline := time.Now().Add(2 * time.Second)
	for i := 0; i < tRaw.NumPieces(); i++ {
		p := tRaw.Piece(i)
		for p.State().Marking && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Send GET /stream?link=HASH&index=1&preload (torrserver uses 1-based index with link)
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/stream?link=%s&index=1&preload", hash), nil)
	rr := httptest.NewRecorder()
	h.Stream(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp torrentResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.PreloadSize != 4096 {
		t.Fatalf("expected PreloadSize 4096, got %d", resp.PreloadSize)
	}
	if resp.PreloadedBytes != 4096 {
		t.Fatalf("expected PreloadedBytes 4096, got %d", resp.PreloadedBytes)
	}

	// Also poll stat: GET /stream?link=HASH&index=1&stat
	statReq := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/stream?link=%s&index=1&stat", hash), nil)
	statRr := httptest.NewRecorder()
	h.Stream(statRr, statReq)

	if statRr.Code != http.StatusOK {
		t.Fatalf("expected stat status 200, got %d", statRr.Code)
	}

	var statResp torrentResponse
	if err := json.NewDecoder(statRr.Body).Decode(&statResp); err != nil {
		t.Fatalf("failed to decode stat response: %v", err)
	}
	if statResp.PreloadedBytes != 4096 {
		t.Fatalf("expected stat PreloadedBytes 4096, got %d", statResp.PreloadedBytes)
	}
}

func TestPreload_UnverifiedFileDoesNotTakeFastPath(t *testing.T) {
	_, _, h, cleanup := setupTorrServerIntegration(t)
	defer cleanup()

	fileData := [][]byte{bytes.Repeat([]byte("U"), 4096)}
	torrentBytes := createTorrserverTestTorrent(1024, "test_preload_unverified", []metainfo.FileInfo{
		{Length: 4096, Path: []string{"video.mp4"}},
	}, fileData)

	tor, err := h.uc.AddTorrentFile(bytes.NewReader(torrentBytes))
	if err != nil {
		t.Fatalf("failed to add torrent: %v", err)
	}
	hash := tor.Hash

	// Request preload for unverified file without writing to disk
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/stream?link=%s&index=1&preload", hash), nil)
	rr := httptest.NewRecorder()
	h.Stream(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp torrentResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Should NOT be immediately 100% preloaded
	if resp.PreloadedBytes == 4096 {
		t.Fatalf("expected unverified file not to be instantly preloaded, got %d", resp.PreloadedBytes)
	}
}
