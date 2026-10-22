package torrserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"torrent-stream-hub/internal/repository"
	"torrent-stream-hub/internal/usecase"
)

func TestUploadPosterFailureDoesNotPanic(t *testing.T) {
	dir, _, h, cleanup := setupTorrServerIntegration(t)
	defer cleanup()
	db, err := repository.NewSQLiteDB(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB().Exec(`CREATE TRIGGER reject_poster BEFORE UPDATE OF poster ON torrents WHEN NEW.poster!='' BEGIN SELECT RAISE(ABORT,'poster failure'); END`); err != nil {
		t.Fatal(err)
	}
	data := createTorrserverTestTorrent(4, "poster-fail", []metainfo.FileInfo{{Length: 4}}, [][]byte{[]byte("data")})
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "poster.torrent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(part, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	w.WriteField("poster", "https://example.invalid/poster.jpg")
	w.Close()
	req := httptest.NewRequest(http.MethodPost, "/torrent/upload", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rr := httptest.NewRecorder()
	h.UploadTorrent(rr, req)
	if rr.Code != 500 {
		t.Fatalf("poster error masked: %d %s", rr.Code, rr.Body.String())
	}
}

func TestPreloadSingleflightCapacityAndTTL(t *testing.T) {
	_, _, h, cleanup := setupTorrServerIntegration(t)
	defer cleanup()
	var files []metainfo.FileInfo
	var contents [][]byte
	for range 5 {
		files = append(files, metainfo.FileInfo{Length: 4, Path: []string{string(rune('a' + len(files)))}})
		contents = append(contents, []byte("data"))
	}
	tor, err := h.uc.AddTorrentFile(bytes.NewReader(createTorrserverTestTorrent(4, "preloads", files, contents)))
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	defer close(release)
	h.warmup = func(ctx context.Context, _ string, _ int, target int64) (int64, int64, error) {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-ctx.Done():
			return 0, target, ctx.Err()
		case <-release:
			return target, target, nil
		}
	}
	request := func(index int) int {
		rr := httptest.NewRecorder()
		h.servePreload(rr, httptest.NewRequest(http.MethodGet, "/stream", nil), tor.Hash, index)
		return rr.Code
	}
	if code := request(0); code != 200 {
		t.Fatal(code)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("warmup not started")
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code := request(0); code != 200 {
				t.Error(code)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("duplicate warmup: %d", calls.Load())
	}
	for i := 1; i < 4; i++ {
		if code := request(i); code != 200 {
			t.Fatal(code)
		}
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("warmup not started")
		}
	}
	if code := request(4); code != 429 {
		t.Fatalf("unbounded warmups: %d", code)
	}
	owner, err := h.uc.GenerationContext(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	key := preloadKey{Hash: tor.Hash, Index: 9}
	h.mu.Lock()
	h.preloads[key] = preloadState{Owner: owner, StartedAt: time.Now().Add(-11 * time.Minute)}
	h.mu.Unlock()
	h.expirePreloads(time.Now())
	h.mu.Lock()
	_, exists := h.preloads[key]
	running := len(h.preloads)
	h.mu.Unlock()
	if exists || running != 4 {
		t.Fatal("TTL failed or removed active task")
	}
}

func TestPreloadFailureAndShutdownReleaseSlots(t *testing.T) {
	_, _, h, cleanup := setupTorrServerIntegration(t)
	defer cleanup()
	tor, err := h.uc.AddTorrentFile(bytes.NewReader(createTorrserverTestTorrent(4, "preload-failure", []metainfo.FileInfo{{Length: 4}}, [][]byte{[]byte("data")})))
	if err != nil {
		t.Fatal(err)
	}
	h.warmup = func(context.Context, string, int, int64) (int64, int64, error) {
		return 0, 4, errors.New("injected read failure")
	}
	request := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.servePreload(rr, httptest.NewRequest(http.MethodGet, "/stream", nil), tor.Hash, 0)
		return rr
	}
	if rr := request(); rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	deadline := time.After(3 * time.Second)
	for {
		h.mu.Lock()
		state := h.preloads[preloadKey{Hash: tor.Hash, Index: 0}]
		h.mu.Unlock()
		if !state.Running {
			break
		}
		select {
		case <-deadline:
			t.Fatal("failed task stuck")
		default:
		}
		time.Sleep(time.Millisecond)
	}
	res := torrentResponse{}
	h.preloadDiagnostic(&res, tor.Hash, 0)
	if res.PreloadStatus != "failed" || res.PreloadError == "" {
		t.Fatal("failed preload reported completion", res)
	}
	entered, cancelled := make(chan struct{}), make(chan struct{})
	h.warmup = func(ctx context.Context, _ string, _ int, target int64) (int64, int64, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		return 0, target, ctx.Err()
	}
	if rr := request(); rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("retry not admitted")
	}
	if err := h.uc.Delete(tor.Hash, false); !errors.Is(err, usecase.ErrStorageConflict) {
		t.Fatal("warmup lost transfer lease", err)
	}
	cleanup()
	select {
	case <-cancelled:
	default:
		t.Fatal("Close did not join warmup")
	}
	if len(h.warmupSlots) != 0 {
		t.Fatal("shutdown retained capacity token")
	}
	if rr := request(); rr.Code != 503 {
		t.Fatal("preload accepted after shutdown", rr.Code)
	}
}

func TestPreloadOldGenerationCannotPublishOverReplacement(t *testing.T) {
	_, _, h, cleanup := setupTorrServerIntegration(t)
	defer cleanup()
	tor, err := h.uc.AddMagnet("magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := h.uc.GenerationContext(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	oldOwner, cancel := context.WithCancel(owner)
	cancel()
	key := preloadKey{Hash: tor.Hash, Index: 0}
	h.mu.Lock()
	h.preloads[key] = preloadState{Owner: owner, Running: true, ReadBytes: 1}
	h.mu.Unlock()
	h.completePreload(key, preloadState{Owner: oldOwner, ReadBytes: 4, StartedAt: time.Now()})
	h.mu.Lock()
	state := h.preloads[key]
	h.mu.Unlock()
	if state.Owner != owner || !state.Running || state.ReadBytes != 1 {
		t.Fatal("old result overwrote replacement generation")
	}
}
