package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/go-chi/chi/v5"
	"torrent-stream-hub/internal/models"
)

func TestRecheckHTTPAcceptsOperationAndRejectsTransfers(t *testing.T) {
	uc, repo, dir, cleanup := setupAPITestUseCase(t)
	defer cleanup()
	hash := writeDownloadMetainfo(t, dir, "check.txt", []metainfo.FileInfo{{Length: 4}}, []byte("safe"))
	if err := repo.SaveTorrent(&models.Torrent{Hash: hash, Name: "check.txt", State: models.StatePaused, Size: 4, Downloaded: 4, Files: []*models.File{{Index: 0, Path: "check.txt", Size: 4, Downloaded: 4}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "check.txt"), []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	activateDownloadFixture(t, uc, repo, dir, hash)
	r := chi.NewRouter()
	NewAPIHandler(uc, nil).RegisterRoutes(r)
	request := func(hash string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/torrent/"+hash+"/action", strings.NewReader(`{"action":"recheck"}`)))
		return rr
	}
	if rr := request("missing"); rr.Code != 404 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	release, err := uc.AcquireTransfer(hash)
	if err != nil {
		t.Fatal(err)
	}
	if rr := request(hash); rr.Code != 409 {
		release()
		t.Fatal("recheck allowed during transfer", rr.Code)
	}
	release()
	rr := request(hash)
	if rr.Code != 202 {
		t.Fatal("recheck still a no-op or rejected", rr.Code, rr.Body.String())
	}
	var op models.RecheckOperation
	if err := json.Unmarshal(rr.Body.Bytes(), &op); err != nil || op.ID == 0 || op.Status == "" {
		t.Fatal("missing operation", err)
	}
	tor, err := uc.GetTorrent(hash)
	if err != nil || tor.Recheck == nil || tor.Recheck.ID != op.ID {
		t.Fatal("operation absent from snapshot", err)
	}
}
