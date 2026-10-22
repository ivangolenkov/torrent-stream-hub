package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/repository"
	"torrent-stream-hub/internal/usecase"
)

func TestDeletionFailureIsVisibleAndCannotResume(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.db")
	db, err := repository.NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e, err := engine.New(&config.Config{DownloadDir: dir, DBPath: path, BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	uc := usecase.NewTorrentUseCase(e, repository.NewTorrentRepo(db))
	if _, err := uc.AddMagnet("magnet:?xt=urn:btih:" + apiTestInfoHash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`CREATE TRIGGER fail_delete BEFORE DELETE ON torrents BEGIN SELECT RAISE(FAIL,'injected deletion failure'); END`); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	NewAPIHandler(uc, nil).RegisterRoutes(r)
	request := func(action string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/torrent/"+apiTestInfoHash+"/action", strings.NewReader(`{"action":"`+action+`","delete_files":false}`)))
		return rr
	}
	if rr := request("delete"); rr.Code != http.StatusInternalServerError {
		t.Fatalf("false delete success: %d %s", rr.Code, rr.Body.String())
	}
	if rr := request("resume"); rr.Code != http.StatusConflict {
		t.Fatalf("pending resumed: %d %s", rr.Code, rr.Body.String())
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/torrents", nil))
	if !strings.Contains(rr.Body.String(), `"deletion_mode":"keep"`) || !strings.Contains(rr.Body.String(), "injected deletion failure") {
		t.Fatalf("missing deletion diagnostics: %s", rr.Body.String())
	}
	if e.GetRawTorrent(apiTestInfoHash) != nil {
		t.Fatal("failed delete retained runtime")
	}
}
