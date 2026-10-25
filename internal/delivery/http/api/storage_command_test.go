package api

import (
	"bytes"
	"encoding/json"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"torrent-stream-hub/internal/models"
)

func TestStorageCommandIdentityAndAcceptedPlan(t *testing.T) {
	uc, repo, _, close := setupAPITestUseCase(t)
	defer close()
	info, err := bencode.Marshal(metainfo.Info{Name: "Original", Length: 4, PieceLength: 4, Pieces: make([]byte, 20)})
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	(&metainfo.MetaInfo{InfoBytes: info}).Write(&encoded)
	tor, err := uc.AddTorrentFile(&encoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := uc.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	saved, err := repo.GetTorrent(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	NewAPIHandler(uc, nil).RegisterRoutes(router)
	request := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/torrent/"+tor.Hash+"/storage", strings.NewReader(body)))
		return rr
	}
	for _, body := range []string{`{"action":"plan_move"}`, `{"action":"bad"}`, `{"action":"plan_move","mapping_revision":1}`, `{`} {
		if rr := request(body); rr.Code != 400 {
			t.Fatal(rr.Code, rr.Body.String())
		}
	}
	cmd := models.StorageCommand{Action: "plan_move", RecordID: saved.Control.RecordID, StorageID: saved.Storage.ID, MappingRevision: saved.Storage.Revision, FolderName: "Moved"}
	bad := cmd
	bad.RecordID = "old-record"
	body, _ := json.Marshal(bad)
	if rr := request(string(body)); rr.Code != 409 || !strings.Contains(rr.Body.String(), `"code":"state_conflict"`) {
		t.Fatal(rr.Code, rr.Body.String())
	}
	body, _ = json.Marshal(cmd)
	rr := request(string(body))
	if rr.Code != 202 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	var location models.StorageLocation
	if err := json.Unmarshal(rr.Body.Bytes(), &location); err != nil {
		t.Fatal(err)
	}
	if location.Operation == nil || location.Operation.Target != filepath.FromSlash("payload/Moved") || location.Operation.Version != 1 {
		t.Fatalf("missing plan identity %+v", location)
	}
	if rr := request(string(body)); rr.Code != 202 {
		t.Fatal("retry created conflict", rr.Code, rr.Body.String())
	}
	rec, _ := repo.Recovery(tor.Hash)
	if rec.Version != 1 {
		t.Fatal("duplicate request created another operation")
	}
}
