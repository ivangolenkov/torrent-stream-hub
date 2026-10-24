package torrserver

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
	"torrent-stream-hub/internal/delivery/http/api"
	"torrent-stream-hub/internal/metafetch"
	"torrent-stream-hub/internal/usecase"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/go-chi/chi/v5"
)

func TestTemporaryAddAllLinkSourcesAndPromotion(t *testing.T) {
	data := createTorrserverTestTorrent(1024, "policy", []metainfo.FileInfo{{Length: 1024, Path: []string{"video.bin"}}}, [][]byte{make([]byte, 1024)})
	mi, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	fileHash := mi.HashInfoBytes().HexString()
	for _, source := range []string{"magnet", "hash", "http-file", "http-magnet", "http-redirect"} {
		t.Run(source, func(t *testing.T) {
			_, repo, h, cleanup := setupTorrServerIntegration(t)
			defer cleanup()
			hash := torrserverTestInfoHash
			link := "magnet:?xt=urn:btih:" + hash
			switch source {
			case "hash":
				link = hash
			case "http-file", "http-magnet", "http-redirect":
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch source {
					case "http-file":
						w.Write(data)
					case "http-magnet":
						w.Write([]byte("magnet:?xt=urn:btih:" + hash))
					case "http-redirect":
						http.Redirect(w, r, "magnet:?xt=urn:btih:"+hash, http.StatusFound)
					}
				}))
				defer server.Close()
				h.fetcher, _ = metafetch.New("127.0.0.1/32")
				link = server.URL
				if source == "http-file" {
					hash = fileHash
				}
			}
			for i, save := range []any{nil, true, false} {
				payload := map[string]any{"action": "add", "link": link, "title": "Keep metadata"}
				if save != nil {
					payload["save_to_db"] = save
				}
				body, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				rr := httptest.NewRecorder()
				h.Torrents(rr, httptest.NewRequest(http.MethodPost, "/torrents", bytes.NewReader(body)))
				if rr.Code != http.StatusOK {
					t.Fatalf("step %d: %d %s", i, rr.Code, rr.Body.String())
				}
				got, err := repo.GetTorrent(hash)
				if err != nil || got == nil || got.IsTemporary != (i == 0) || got.Title != "Keep metadata" {
					t.Fatalf("step %d: %+v %v", i, got, err)
				}
			}
		})
	}
}

type heldResponse struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	proceed chan struct{}
	once    sync.Once
}

func (w *heldResponse) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.proceed })
	return w.ResponseRecorder.Write(data)
}

func TestTemporaryHTTPTransfersBlockCleanup(t *testing.T) {
	for _, mode := range []string{"disk-stream", "reader-stream", "file-download", "torrent-download", "preload"} {
		t.Run(mode, func(t *testing.T) {
			dir, repo, h, cleanup := setupTorrServerIntegration(t)
			defer cleanup()
			data := bytes.Repeat([]byte{17}, 4096)
			oldBytes := createTorrserverTestTorrent(1024, "old", []metainfo.FileInfo{{Length: int64(len(data)), Path: []string{"video.mp4"}}}, [][]byte{data})
			old, err := h.uc.AddTorrentFileWithPolicy(bytes.NewReader(oldBytes), usecase.TorrentMetadata{}, true)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "payload", old.Hash, "old", "video.mp4")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := h.uc.Recheck(old.Hash); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				ready, _, err := h.uc.CheckFileDirectReady(old.Hash, 0)
				if err != nil {
					t.Fatal(err)
				}
				if ready {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("file did not become ready")
				}
				time.Sleep(time.Millisecond)
			}
			if mode == "reader-stream" {
				// A lingering .part forces the torrent-reader fallback, even though
				// the verified pieces can already satisfy the read.
				if err := os.WriteFile(path+".part", data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			router := chi.NewRouter()
			h.RegisterRoutes(router)
			api.NewAPIHandler(h.uc, nil).RegisterRoutes(router)
			url := "/stream?link=" + old.Hash + "&index=1"
			switch mode {
			case "file-download":
				url = "/torrent/" + old.Hash + "/file/0/download"
			case "torrent-download":
				url = "/torrent/" + old.Hash + "/download"
			case "preload":
				url += "&preload"
			}
			w := &heldResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), proceed: make(chan struct{})}
			var unblock sync.Once
			release := func() { unblock.Do(func() { close(w.proceed) }) }
			defer release()
			done := make(chan struct{})
			go func() { defer close(done); router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil)) }()
			select {
			case <-w.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("HTTP transfer did not start")
			}
			if err := h.uc.SetTemporaryLimit(1); err != nil {
				t.Fatal(err)
			}
			newBytes := createTorrserverTestTorrent(1024, "new", []metainfo.FileInfo{{Length: 1024, Path: []string{"new.bin"}}}, [][]byte{make([]byte, 1024)})
			if _, err := h.uc.AddTorrentFileWithPolicy(bytes.NewReader(newBytes), usecase.TorrentMetadata{}, true); err != nil {
				t.Fatal(err)
			}
			if got, err := repo.GetTorrent(old.Hash); err != nil || got == nil {
				t.Fatalf("active transfer was evicted: %v %v", got, err)
			}
			release()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("HTTP transfer did not finish")
			}
			if w.Code != http.StatusOK {
				t.Fatal(w.Code, w.Body.String())
			}
			h.uc.CleanupTemporary("debounce-test")
			if got, err := repo.GetTorrent(old.Hash); err != nil || got == nil {
				t.Fatal("HTTP debounce was ignored")
			}
		})
	}
}

func TestUploadPromotesExistingTemporaryTorrent(t *testing.T) {
	_, repo, h, cleanup := setupTorrServerIntegration(t)
	defer cleanup()
	data := createTorrserverTestTorrent(1024, "upload", []metainfo.FileInfo{{Length: 1024, Path: []string{"video.bin"}}}, [][]byte{make([]byte, 1024)})
	torrent, err := h.uc.AddTorrentFileWithPolicy(bytes.NewReader(data), usecase.TorrentMetadata{}, true)
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	file, err := form.CreateFormFile("file", "upload.torrent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/torrent/upload", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rr := httptest.NewRecorder()
	h.UploadTorrent(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatal(rr.Code, rr.Body.String())
	}
	got, err := repo.GetTorrent(torrent.Hash)
	if err != nil || got == nil || got.IsTemporary {
		t.Fatalf("upload did not promote: %+v %v", got, err)
	}
}
