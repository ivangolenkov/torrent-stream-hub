package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"torrent-stream-hub/internal/delivery/http/torrserver"
	"torrent-stream-hub/internal/usecase"
)

type pausedTransferWriter struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	proceed chan struct{}
	once    sync.Once
}

func (w *pausedTransferWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.proceed })
	return w.ResponseRecorder.Write(p)
}

// Run against the actual verified multifile fixture: an archive occupies the
// last shared slot, so native file, archive and TorrServer all reject N+1 before
// touching their data source. HEAD still succeeds and release permits a GET.
func assertSharedTransferBudget(t *testing.T, uc *usecase.TorrentUseCase, hash string) {
	t.Helper()
	for i := 0; i < 3; i++ {
		release, err := uc.AcquireHTTPTransfer(hash, context.Background(), false)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}
	router := chi.NewRouter()
	NewAPIHandler(uc, nil).RegisterRoutes(router)
	torrserver.NewTorrServerHandler(uc).RegisterRoutes(router)
	archive := "/torrent/" + hash + "/download"
	file := "/torrent/" + hash + "/file/0/download"
	stream := "/stream?link=" + hash + "&index=1"
	writer := &pausedTransferWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), proceed: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); router.ServeHTTP(writer, httptest.NewRequest("GET", archive, nil)) }()
	var unblock sync.Once
	defer func() { unblock.Do(func() { close(writer.proceed) }); <-done }()
	select {
	case <-writer.entered:
	case <-done:
		t.Fatal("archive never wrote", writer.Code)
	case <-time.After(3 * time.Second):
		t.Fatal("archive preparation stuck")
	}
	for _, path := range []string{archive, file, stream} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("%s bypassed shared budget: %d %s", path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{archive, file, stream} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("HEAD", path, nil))
		if w.Code != http.StatusOK || w.Body.Len() != 0 {
			t.Fatalf("HEAD %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	unblock.Do(func() { close(writer.proceed) })
	<-done
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", file, nil))
	if w.Code != http.StatusOK {
		t.Fatal("archive leaked slot", w.Code, w.Body.String())
	}
}
