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
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
	"torrent-stream-hub/internal/usecase"
)

func TestPriorityHTTPValidation(t *testing.T) {
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
	repo := repository.NewTorrentRepo(db)
	uc := usecase.NewTorrentUseCase(e, repo)
	pending, err := uc.AddMagnet("magnet:?xt=urn:btih:" + apiTestInfoHash)
	if err != nil {
		t.Fatal(err)
	}
	known := "abcdef0123456789abcdef0123456789abcdef0123"
	if err := repo.SaveTorrent(&models.Torrent{Hash: known, State: models.StatePaused, Files: []*models.File{{Index: 0, Path: "video"}}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveStorageMapping(&repository.StorageMapping{Hash: known, Version: 1, Root: "payload/" + known, State: "ready"}); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	NewAPIHandler(uc, nil).RegisterRoutes(r)
	for _, tc := range []struct {
		path, body string
		code       int
	}{
		{"/torrent/" + known + "/priority", `{}`, 400},
		{"/torrent/" + known + "/priority", `{"priority":null}`, 400},
		{"/torrent/" + known + "/priority", `{"priority":99}`, 400},
		{"/torrent/" + known + "/file/-1/priority", `{"priority":0}`, 400},
		{"/torrent/missing/priority", `{"priority":0}`, 404},
		{"/torrent/" + known + "/file/1/priority", `{"priority":0}`, 404},
		{"/torrent/" + pending.Hash + "/file/0/priority", `{"priority":0}`, 409},
		{"/torrent/" + pending.Hash + "/priority", `{"priority":0}`, 409},
		{"/torrent/" + known + "/file/0/priority", `{"priority":-1}`, 200},
		{"/torrent/" + known + "/file/0/priority", `{"priority":-1}`, 200},
	} {
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, httptest.NewRequest(http.MethodPut, tc.path, strings.NewReader(tc.body)))
		if rr.Code != tc.code {
			t.Errorf("%s %s: want %d got %d: %s", tc.path, tc.body, tc.code, rr.Code, rr.Body.String())
		}
	}
	if err := repo.BeginForget(known); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodPut, "/torrent/"+known+"/priority", strings.NewReader(`{"priority":1}`)))
	if rr.Code != 409 {
		t.Fatalf("pending deletion accepted priority: %d", rr.Code)
	}
}
