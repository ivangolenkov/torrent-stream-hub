package api

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/go-chi/chi/v5"
	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/fastresume"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
	"torrent-stream-hub/internal/usecase"
)

func TestResumeRuntimeHTTPRangeAfterAutomaticShutdownSeal(t *testing.T) {
	dir := t.TempDir()
	var storageCalls atomic.Int64
	open := func(reads *atomic.Int64) (*usecase.TorrentUseCase, *repository.TorrentRepo, *engine.Engine, func()) {
		t.Helper()
		db, err := repository.NewSQLiteDB(filepath.Join(dir, "hub.db"))
		if err != nil {
			t.Fatal(err)
		}
		e, err := engine.New(&config.Config{DownloadDir: dir, DBPath: filepath.Join(dir, "hub.db"), TorrentPort: 0,
			BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true, BTDisableUTP: true, BTRetrackersMode: "off"},
			engine.WithStorageIOObserver(func(op, _ string, n int64) {
				storageCalls.Add(1)
				if reads != nil && op == "read" {
					reads.Add(n)
				}
			}))
		if err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		repo := repository.NewTorrentRepo(db)
		uc, err := usecase.NewTorrentUseCaseWithResumeMode(e, repo, fastresume.Fast)
		if err != nil {
			e.Close()
			_ = db.Close()
			t.Fatal(err)
		}
		close := func() { e.Close(); _ = db.Close() }
		t.Cleanup(close)
		return uc, repo, e, close
	}
	warm, repo, e, closeWarm := open(nil)
	data := []byte("hello downloaded file")
	sum := sha1.Sum(data)
	info := &metainfo.Info{Name: "movie.mkv", PieceLength: 16384, Length: int64(len(data)), Pieces: sum[:]}
	encoded, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: encoded}
	var input bytes.Buffer
	if err := mi.Write(&input); err != nil {
		t.Fatal(err)
	}
	tor, err := warm.AddTorrentFile(&input)
	if err != nil {
		t.Fatal(err)
	}
	if err := warm.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	m, _ := repo.StorageMapping(tor.Hash)
	file := info.UpvertedFiles()[0]
	path, err := m.Layout().File(info, &file)
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.GetRawTorrent(tor.Hash).VerifyDataContext(ctx); err != nil {
		t.Fatal(err)
	}
	closeWarm()
	var reads atomic.Int64
	cold, _, _, closeCold := open(&reads)
	defer closeCold()
	if err := cold.RestoreTorrents(); err != nil {
		t.Fatal(err)
	}
	h := NewAPIHandler(cold, nil)
	router := chi.NewRouter()
	h.RegisterRoutes(router)
	beforePoll := storageCalls.Load()
	for _, url := range []string{"/torrent/" + tor.Hash, "/torrents?view=compact"} {
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, url, nil))
		if rr.Code != http.StatusOK {
			t.Fatal(rr.Code, rr.Body.String())
		}
		var view models.Torrent
		if url[0:9] == "/torrents" {
			var items []*models.Torrent
			if err := json.Unmarshal(rr.Body.Bytes(), &items); err != nil || len(items) != 1 {
				t.Fatal(err, rr.Body.String())
			}
			view = *items[0]
			if view.Files != nil {
				t.Fatal("compact duplicated catalog")
			}
		} else if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		if view.Resume == nil || view.Resume.RestoredBytes != int64(len(data)) || view.Resume.FreshlyVerifiedBytes != 0 || view.Verification.CheckedBytes != 0 {
			t.Fatalf("REST misrepresents restore: %+v", view.Resume)
		}
	}
	if storageCalls.Load() != beforePoll {
		t.Fatal("API polling performed storage I/O", beforePoll, storageCalls.Load())
	}
	req := httptest.NewRequest(http.MethodGet, "/torrent/"+tor.Hash+"/file/0/download", nil)
	req.Header.Set("Range", "bytes=6-15")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusPartialContent || rr.Body.String() != "downloaded" || rr.Header().Get("Content-Range") != "bytes 6-15/21" {
		t.Fatal(rr.Code, rr.Body.String(), rr.Header())
	}
	if reads.Load() != 0 {
		t.Fatal("HTTP readiness hashed restored payload", reads.Load())
	}
}
