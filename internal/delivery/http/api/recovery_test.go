package api

import (
	"encoding/json"
	"fmt"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
	"torrent-stream-hub/internal/usecase"
)

func TestRecoveryDeletionActionContract(t *testing.T) {
	dir := t.TempDir()
	db, err := repository.NewSQLiteDB(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e, err := engine.New(&config.Config{DownloadDir: dir, DBPath: filepath.Join(dir, "hub.db"), BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true, BTRetrackersMode: "off"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	repo := repository.NewTorrentRepo(db)
	uc := usecase.NewTorrentUseCase(e, repo)
	if err := repo.SaveTorrent(&models.Torrent{Hash: apiTestInfoHash, State: models.StatePaused}); err != nil {
		t.Fatal(err)
	}
	if err := repo.BeginCleanup(apiTestInfoHash, []string{"remaining"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveDeletionError(apiTestInfoHash, fmt.Errorf("original unlink failure")); err != nil {
		t.Fatal(err)
	}
	rec, _ := repo.Recovery(apiTestInfoHash)
	router := chi.NewRouter()
	NewAPIHandler(uc, nil).RegisterRoutes(router)
	action := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/torrent/"+apiTestInfoHash+"/action", strings.NewReader(body)))
		return rr
	}
	rr := action(`{"action":"delete","delete_files":false}`)
	if rr.Code != 409 {
		t.Fatalf("mode mismatch: %d %s", rr.Code, rr.Body.String())
	}
	var mismatch models.StorageActionError
	if err := json.Unmarshal(rr.Body.Bytes(), &mismatch); err != nil {
		t.Fatal(err)
	}
	if mismatch.Code != models.StorageDeletionMode || mismatch.StorageRecovery == nil || mismatch.StorageRecovery.Deletion.Mode != "payload" {
		t.Fatalf("typed recovery missing: %+v", mismatch)
	}
	if mismatch.StorageRecovery.Deletion.Error != "original unlink failure" {
		t.Fatal("invalid mode request replaced original deletion failure")
	}
	rr = action(fmt.Sprintf(`{"action":"cancel_deletion","record_id":%q,"version":"999"}`, rec.RecordID))
	if rr.Code != 409 {
		t.Fatal("stale operation accepted", rr.Code)
	}
	rr = action(fmt.Sprintf(`{"action":"cancel_deletion","record_id":%q,"version":"%d"}`, rec.RecordID, rec.Version))
	if rr.Code != 200 {
		t.Fatalf("cancel: %d %s", rr.Code, rr.Body.String())
	}
	details := httptest.NewRecorder()
	router.ServeHTTP(details, httptest.NewRequest(http.MethodGet, "/torrent/"+apiTestInfoHash, nil))
	var tor models.Torrent
	if err := json.Unmarshal(details.Body.Bytes(), &tor); err != nil {
		t.Fatal(err)
	}
	if tor.StorageRecovery == nil || tor.StorageRecovery.Reason != models.StorageDeletionCancelled || tor.Control.DesiredRun != models.RunPaused {
		t.Fatalf("details missing cancelled state: %s", details.Body.String())
	}
	rr = action(fmt.Sprintf(`{"action":"retry_deletion","record_id":%q,"version":"%d"}`, rec.RecordID, rec.Version))
	if rr.Code != 409 {
		t.Fatal("cancelled deletion revived", rr.Code)
	}
}
