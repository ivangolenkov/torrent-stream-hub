package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/repository"
	"torrent-stream-hub/internal/usecase"

	"github.com/go-chi/chi/v5"
)

func TestMakePermanentAPI(t *testing.T) {
	dir := t.TempDir()
	db, err := repository.NewSQLiteDB(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	eng, err := engine.New(&config.Config{DBPath: filepath.Join(dir, "hub.db"), DownloadDir: dir, TorrentPort: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	repo := repository.NewTorrentRepo(db)
	uc := usecase.NewTorrentUseCase(eng, repo)
	magnet := "magnet:?xt=urn:btih:" + apiTestInfoHash
	if _, err := uc.AddMagnetWithPolicy(magnet, usecase.TorrentMetadata{}, true); err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	NewAPIHandler(uc, nil).RegisterRoutes(router)
	for _, tc := range []struct {
		hash string
		want int
	}{{apiTestInfoHash, 200}, {apiTestInfoHash, 200}, {"missing", 404}} {
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/torrent/"+tc.hash+"/action", strings.NewReader(`{"action":"make_permanent"}`)))
		if rr.Code != tc.want {
			t.Fatalf("status %d want %d: %s", rr.Code, tc.want, rr.Body.String())
		}
	}
	// A native add also promotes an existing temporary torrent.
	secondHash := "abcdef0123456789abcdef0123456789abcdef01"
	if _, err := uc.AddMagnetWithPolicy("magnet:?xt=urn:btih:"+secondHash, usecase.TorrentMetadata{}, true); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/torrent/add", strings.NewReader(`{"link":"magnet:?xt=urn:btih:`+secondHash+`"}`)))
	if rr.Code != http.StatusAccepted {
		t.Fatal(rr.Code, rr.Body.String())
	}
	got, _ := repo.GetTorrent(secondHash)
	if got == nil || got.IsTemporary {
		t.Fatal("native add did not promote")
	}
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/torrents", nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"is_temporary":false`) {
		t.Fatal(rr.Code, rr.Body.String())
	}
	db.Close()
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/torrent/"+apiTestInfoHash+"/action", strings.NewReader(`{"action":"make_permanent"}`)))
	if rr.Code != http.StatusInternalServerError {
		t.Fatal("DB error was hidden", rr.Code)
	}
}
