package api

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"io"
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
	"github.com/anacrolix/torrent/storage"
	"github.com/go-chi/chi/v5"
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

func TestAddTorrentPersistsPoster(t *testing.T) {
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
	h := NewAPIHandler(uc, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/torrent/add", strings.NewReader(`{"link":"magnet:?xt=urn:btih:`+apiTestInfoHash+`","poster":"https://example.com/poster.jpg"}`))
	rr := httptest.NewRecorder()

	h.AddTorrent(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, rr.Code, rr.Body.String())
	}
	fetched, err := repo.GetTorrent(apiTestInfoHash)
	if err != nil {
		t.Fatalf("failed to get torrent: %v", err)
	}
	if fetched == nil || fetched.Poster != "https://example.com/poster.jpg" {
		t.Fatalf("expected poster to be persisted, got %+v", fetched)
	}
}

func TestBTHealthReturnsDiagnostics(t *testing.T) {
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
	h := NewAPIHandler(uc, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health/bt", nil)
	rr := httptest.NewRecorder()

	h.BTHealth(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if body["seed_enabled"] != true {
		t.Fatalf("expected seed_enabled=true, got %#v", body["seed_enabled"])
	}
	if body["upload_enabled"] != true {
		t.Fatalf("expected upload_enabled=true, got %#v", body["upload_enabled"])
	}
	if _, ok := body["incoming_connectivity_note"]; !ok {
		t.Fatalf("expected incoming connectivity note in response: %#v", body)
	}
	if body["swarm_watchdog_enabled"] != true {
		t.Fatalf("expected swarm watchdog to be enabled, got %#v", body["swarm_watchdog_enabled"])
	}
	if body["swarm_check_interval_sec"] == nil || body["swarm_refresh_cooldown_sec"] == nil {
		t.Fatalf("expected swarm watchdog timings in response: %#v", body)
	}
	if body["peer_drop_ratio"] == nil || body["seed_drop_ratio"] == nil || body["speed_drop_ratio"] == nil {
		t.Fatalf("expected trend ratios in response: %#v", body)
	}
	encoded := rr.Body.String()
	if strings.Contains(encoded, "peer_ip") || strings.Contains(encoded, "peer_port") {
		t.Fatalf("health response must not expose peer IP/ports: %s", encoded)
	}
}

func TestDownloadTorrentFileServesCompletedFileWithRange(t *testing.T) {
	uc, repo, downloadDir, cleanup := setupAPITestUseCase(t)
	defer cleanup()

	content := []byte("hello downloaded file")
	apiTestInfoHash := writeDownloadMetainfo(t, downloadDir, "movie.mkv", []metainfo.FileInfo{{Length: int64(len(content))}}, content)
	filePath := filepath.Join(downloadDir, "movie.mkv")
	if err := os.WriteFile(filePath, content, 0o600); err != nil {
		t.Fatalf("failed to create downloaded file: %v", err)
	}
	if err := repo.SaveTorrent(&models.Torrent{
		Hash:       apiTestInfoHash,
		Name:       "movie.mkv",
		Size:       int64(len(content)),
		Downloaded: int64(len(content)),
		State:      models.StateSeeding,
		Files: []*models.File{{
			Index:      0,
			Path:       "movie.mkv",
			Size:       int64(len(content)),
			Downloaded: int64(len(content)),
		}},
	}); err != nil {
		t.Fatalf("failed to save torrent: %v", err)
	}

	activateDownloadFixture(t, uc, repo, downloadDir, apiTestInfoHash)
	h := NewAPIHandler(uc, nil)
	router := chi.NewRouter()
	h.RegisterRoutes(router)
	req := httptest.NewRequest(http.MethodGet, "/torrent/"+apiTestInfoHash+"/file/0/download", nil)
	req.Header.Set("Range", "bytes=6-15")
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusPartialContent {
		t.Fatalf("expected status %d, got %d: %s", http.StatusPartialContent, rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != "downloaded" {
		t.Fatalf("expected ranged body %q, got %q", "downloaded", got)
	}
	if disposition := rr.Header().Get("Content-Disposition"); !strings.Contains(disposition, "movie.mkv") {
		t.Fatalf("expected content disposition filename, got %q", disposition)
	}
}

func TestDownloadTorrentFileRejectsIncompleteFile(t *testing.T) {
	uc, repo, _, cleanup := setupAPITestUseCase(t)
	defer cleanup()

	if err := repo.SaveTorrent(&models.Torrent{
		Hash:       apiTestInfoHash,
		Name:       "movie.mkv",
		Size:       100,
		Downloaded: 50,
		State:      models.StateDownloading,
		Files: []*models.File{{
			Index:      0,
			Path:       "movie.mkv",
			Size:       100,
			Downloaded: 50,
		}},
	}); err != nil {
		t.Fatalf("failed to save torrent: %v", err)
	}

	h := NewAPIHandler(uc, nil)
	router := chi.NewRouter()
	h.RegisterRoutes(router)
	req := httptest.NewRequest(http.MethodGet, "/torrent/"+apiTestInfoHash+"/file/0/download", nil)
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	assertJSONError(t, rr, http.StatusConflict, "Download is not ready")
}

func TestDownloadSingleFileTorrentServesFileNotZip(t *testing.T) {
	uc, repo, downloadDir, cleanup := setupAPITestUseCase(t)
	defer cleanup()

	content := []byte("single file torrent")
	apiTestInfoHash := writeDownloadMetainfo(t, downloadDir, "single.txt", []metainfo.FileInfo{{Length: int64(len(content))}}, content)
	if err := os.WriteFile(filepath.Join(downloadDir, "single.txt"), content, 0o600); err != nil {
		t.Fatalf("failed to create downloaded file: %v", err)
	}
	if err := repo.SaveTorrent(&models.Torrent{
		Hash:       apiTestInfoHash,
		Name:       "single.txt",
		Size:       int64(len(content)),
		Downloaded: int64(len(content)),
		State:      models.StateSeeding,
		Files: []*models.File{{
			Index:      0,
			Path:       "single.txt",
			Size:       int64(len(content)),
			Downloaded: int64(len(content)),
		}},
	}); err != nil {
		t.Fatalf("failed to save torrent: %v", err)
	}

	activateDownloadFixture(t, uc, repo, downloadDir, apiTestInfoHash)
	h := NewAPIHandler(uc, nil)
	router := chi.NewRouter()
	h.RegisterRoutes(router)
	req := httptest.NewRequest(http.MethodGet, "/torrent/"+apiTestInfoHash+"/download", nil)
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rr.Code, rr.Body.String())
	}
	if !bytes.Equal(rr.Body.Bytes(), content) {
		t.Fatalf("expected single file body %q, got %q", content, rr.Body.Bytes())
	}
	if contentType := rr.Header().Get("Content-Type"); strings.Contains(contentType, "zip") {
		t.Fatalf("expected non-zip content type, got %q", contentType)
	}
}

func TestDownloadMultiFileTorrentStreamsZip(t *testing.T) {
	uc, repo, downloadDir, cleanup := setupAPITestUseCase(t)
	defer cleanup()

	root := filepath.Join(downloadDir, "Torrent Root")
	if err := os.MkdirAll(filepath.Join(root, "Season 1"), 0o700); err != nil {
		t.Fatalf("failed to create dirs: %v", err)
	}
	files := map[string][]byte{
		"Season 1/episode1.txt": []byte("episode one"),
		"Season 1/episode2.txt": []byte("episode two"),
	}
	var modelFiles []*models.File
	var total int64
	index := 0
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), content, 0o600); err != nil {
			t.Fatalf("failed to create file %s: %v", rel, err)
		}
		modelFiles = append(modelFiles, &models.File{Index: index, Path: rel, Size: int64(len(content)), Downloaded: int64(len(content))})
		total += int64(len(content))
		index++
	}
	var metadataFiles []metainfo.FileInfo
	for _, file := range modelFiles {
		metadataFiles = append(metadataFiles, metainfo.FileInfo{Length: file.Size, Path: strings.Split(file.Path, "/")})
	}
	apiTestInfoHash := writeDownloadMetainfo(t, downloadDir, "Torrent Root", metadataFiles)
	if err := repo.SaveTorrent(&models.Torrent{
		Hash:       apiTestInfoHash,
		Name:       "Torrent Root",
		Size:       total,
		Downloaded: total,
		State:      models.StateSeeding,
		Files:      modelFiles,
	}); err != nil {
		t.Fatalf("failed to save torrent: %v", err)
	}
	activateDownloadFixture(t, uc, repo, downloadDir, apiTestInfoHash)

	h := NewAPIHandler(uc, nil)
	router := chi.NewRouter()
	h.RegisterRoutes(router)
	req := httptest.NewRequest(http.MethodGet, "/torrent/"+apiTestInfoHash+"/download", nil)
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rr.Code, rr.Body.String())
	}
	if contentType := rr.Header().Get("Content-Type"); contentType != "application/zip" {
		t.Fatalf("expected zip content type, got %q", contentType)
	}
	zipReader, err := zip.NewReader(bytes.NewReader(rr.Body.Bytes()), int64(rr.Body.Len()))
	if err != nil {
		t.Fatalf("failed to read zip: %v", err)
	}
	if len(zipReader.File) != len(files) {
		t.Fatalf("expected %d zip entries, got %d", len(files), len(zipReader.File))
	}
	for _, entry := range zipReader.File {
		expected, ok := files[entry.Name]
		if !ok {
			t.Fatalf("unexpected zip entry %q", entry.Name)
		}
		in, err := entry.Open()
		if err != nil {
			t.Fatalf("failed to open zip entry %q: %v", entry.Name, err)
		}
		actual, err := io.ReadAll(in)
		in.Close()
		if err != nil {
			t.Fatalf("failed to read zip entry %q: %v", entry.Name, err)
		}
		if !bytes.Equal(actual, expected) {
			t.Fatalf("entry %q expected %q, got %q", entry.Name, expected, actual)
		}
	}
}

func TestDownloadMultiFileTorrentRejectsRangeForZip(t *testing.T) {
	uc, repo, downloadDir, cleanup := setupAPITestUseCase(t)
	defer cleanup()

	root := filepath.Join(downloadDir, "Torrent Root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatalf("failed to create file %s: %v", name, err)
		}
	}
	apiTestInfoHash := writeDownloadMetainfo(t, downloadDir, "Torrent Root", []metainfo.FileInfo{{Length: 5, Path: []string{"a.txt"}}, {Length: 5, Path: []string{"b.txt"}}})
	if err := repo.SaveTorrent(&models.Torrent{
		Hash:       apiTestInfoHash,
		Name:       "Torrent Root",
		Size:       10,
		Downloaded: 10,
		State:      models.StateSeeding,
		Files: []*models.File{
			{Index: 0, Path: "a.txt", Size: 5, Downloaded: 5},
			{Index: 1, Path: "b.txt", Size: 5, Downloaded: 5},
		},
	}); err != nil {
		t.Fatalf("failed to save torrent: %v", err)
	}

	h := NewAPIHandler(uc, nil)
	router := chi.NewRouter()
	h.RegisterRoutes(router)
	req := httptest.NewRequest(http.MethodGet, "/torrent/"+apiTestInfoHash+"/download", nil)
	activateDownloadFixture(t, uc, repo, downloadDir, apiTestInfoHash)
	req.Header.Set("Range", "bytes=0-10")
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	assertJSONError(t, rr, http.StatusRequestedRangeNotSatisfiable, "Range is not supported for zip downloads")
}

func TestDownloadTorrentRejectsUnsafePath(t *testing.T) {
	uc, repo, _, cleanup := setupAPITestUseCase(t)
	defer cleanup()

	if err := repo.SaveTorrent(&models.Torrent{
		Hash:       apiTestInfoHash,
		Name:       "unsafe",
		Size:       4,
		Downloaded: 4,
		State:      models.StateSeeding,
		Files: []*models.File{{
			Index:      0,
			Path:       "../outside.txt",
			Size:       4,
			Downloaded: 4,
		}},
	}); err != nil {
		t.Fatalf("failed to save torrent: %v", err)
	}

	h := NewAPIHandler(uc, nil)
	router := chi.NewRouter()
	h.RegisterRoutes(router)
	req := httptest.NewRequest(http.MethodGet, "/torrent/"+apiTestInfoHash+"/download", nil)
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	assertJSONError(t, rr, http.StatusConflict, "Download is not ready")
}

// Completed legacy fixtures must carry real metainfo, not guessed DB paths.
func writeDownloadMetainfo(t *testing.T, dir, name string, files []metainfo.FileInfo, contents ...[]byte) string {
	t.Helper()
	info := metainfo.Info{Name: name, PieceLength: 1024}
	var size int64
	for _, f := range files {
		size += f.Length
	}
	if len(files) == 1 && len(files[0].Path) == 0 {
		info.Length = size
	} else {
		info.Files = files
	}
	var content []byte
	if len(contents) > 0 {
		content = bytes.Join(contents, nil)
	} else {
		for _, file := range files {
			path := filepath.Join(append([]string{dir, name}, file.Path...)...)
			data, _ := os.ReadFile(path)
			content = append(content, data...)
		}
	}
	for offset := int64(0); offset < size; offset += 1024 {
		end := min(offset+1024, size)
		chunk := make([]byte, end-offset)
		if offset < int64(len(content)) {
			copy(chunk, content[offset:min(end, int64(len(content)))])
		}
		h := sha1.Sum(chunk)
		info.Pieces = append(info.Pieces, h[:]...)
	}
	data, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: data}
	if err := os.MkdirAll(filepath.Join(dir, "metainfo"), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "metainfo", mi.HashInfoBytes().HexString()+".torrent"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := mi.Write(f); err != nil {
		t.Fatal(err)
	}
	return mi.HashInfoBytes().HexString()
}

func activateDownloadFixture(t *testing.T, uc *usecase.TorrentUseCase, repo *repository.TorrentRepo, dir, hash string) string {
	t.Helper()
	mi, err := metainfo.LoadFromFile(filepath.Join(dir, "metainfo", hash+".torrent"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("payload", hash)
	if err := repo.SaveStorageMapping(&repository.StorageMapping{Hash: hash, Version: 1, Root: rel, State: "ready"}); err != nil {
		t.Fatal(err)
	}
	for _, file := range info.UpvertedFiles() {
		path := storage.DefaultFilePathMaker(storage.FilePathMakerOpts{Info: &info, File: &file})
		for _, suffix := range []string{"", ".part"} {
			from, to := filepath.Join(dir, path+suffix), filepath.Join(dir, rel, path+suffix)
			if _, err := os.Lstat(from); os.IsNotExist(err) {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(to), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(from, to); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := uc.RestoreTorrents(); err != nil {
		t.Fatal(err)
	}
	f, err := uc.GetTorrentFile(hash, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Torrent().VerifyData(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		all := true
		for index := range info.UpvertedFiles() {
			ready, _, err := uc.CheckFileDirectReady(hash, index)
			if err != nil {
				t.Fatal(err)
			}
			all = all && ready
		}
		if all {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture not verified")
		}
		time.Sleep(time.Millisecond)
	}
	return filepath.Join(dir, rel)
}

func setupAPITestUseCase(t *testing.T) (*usecase.TorrentUseCase, *repository.TorrentRepo, string, func()) {
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
	return usecase.NewTorrentUseCase(eng, repo), repo, dir, func() {
		eng.Close()
		db.Close()
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
