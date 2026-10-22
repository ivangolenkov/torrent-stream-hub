package api

import (
	"context"
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

func TestResumeDiskConflictAndDatabaseError(t *testing.T) {
	dir := t.TempDir()
	db, err := repository.NewSQLiteDB(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e, err := engine.New(&config.Config{DownloadDir: dir, DBPath: filepath.Join(dir, "hub.db"), MinFreeSpaceGB: 1_000_000_000, BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	uc := usecase.NewTorrentUseCase(e, repository.NewTorrentRepo(db))
	if _, err := uc.AddMagnet("magnet:?xt=urn:btih:" + apiTestInfoHash); err != nil {
		t.Fatal(err)
	}
	h := NewAPIHandler(uc, nil)
	request := func(hash string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/torrent/"+hash+"/action", strings.NewReader(`{"action":"resume"}`))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("hash", hash)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		rr := httptest.NewRecorder()
		h.TorrentAction(rr, req)
		return rr
	}
	rr := request(apiTestInfoHash)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "reserve") {
		t.Fatalf("disk conflict: %d %s", rr.Code, rr.Body.String())
	}
	if rr := request("missing"); rr.Code != http.StatusNotFound {
		t.Fatalf("missing: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := db.DB().Exec(`CREATE TRIGGER reject_resume BEFORE UPDATE ON torrents BEGIN SELECT RAISE(FAIL,'injected DB error'); END`); err != nil {
		t.Fatal(err)
	}
	if rr := request(apiTestInfoHash); rr.Code != http.StatusInternalServerError {
		t.Fatalf("database error: %d %s", rr.Code, rr.Body.String())
	}
	if e.GetRawTorrent(apiTestInfoHash).DataDownloadAllowed() {
		t.Fatal("HTTP failure opened download gate")
	}
}
