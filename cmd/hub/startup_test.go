package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"torrent-stream-hub/internal/config"
	deliveryhttp "torrent-stream-hub/internal/delivery/http"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"
	"torrent-stream-hub/internal/usecase"
)

func TestHTTPAvailableDuringPersistedRestore(t *testing.T) {
	for _, finish := range []bool{false, true} {
		name := "cancel during restore"
		if finish {
			name = "finish restore"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			db, err := repository.NewSQLiteDB(filepath.Join(dir, "hub.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := os.MkdirAll(filepath.Join(dir, "downloads"), 0700); err != nil {
				t.Fatal(err)
			}
			eng, err := engine.New(&config.Config{
				DBPath: filepath.Join(dir, "hub.db"), DownloadDir: filepath.Join(dir, "downloads"),
				MaxActiveDownloads: 1, BTDisableDHT: true, BTDisablePEX: true,
				BTDisableUPNP: true, BTDisableUTP: true, BTRetrackersMode: "off",
			})
			if err != nil {
				t.Fatal(err)
			}
			defer eng.Close()
			repo := repository.NewTorrentRepo(db)
			uc := usecase.NewTorrentUseCase(eng, repo)
			info, err := bencode.Marshal(metainfo.Info{Name: "persisted", Length: 4, PieceLength: 4, Pieces: make([]byte, 20)})
			if err != nil {
				t.Fatal(err)
			}
			mi := metainfo.MetaInfo{InfoBytes: info}
			hash := mi.HashInfoBytes().HexString()
			if err := repo.SaveTorrent(&models.Torrent{Hash: hash, Name: "persisted", State: models.StateQueued}); err != nil {
				t.Fatal(err)
			}
			if err := repo.EnsureStorageMapping(&repository.StorageMapping{Hash: hash, Version: payload.LayoutVersion, Root: payload.Root(hash), State: "ready"}); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, "metainfo"), 0700); err != nil {
				t.Fatal(err)
			}
			encoded, err := bencode.Marshal(mi)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "metainfo", hash+".torrent"), encoded, 0600); err != nil {
				t.Fatal(err)
			}

			// Block the actual restore while opening storage under the library lock.
			// HTTP diagnostics must not depend on acquiring that lock.
			entered, release := make(chan struct{}), make(chan struct{})
			var enteredOnce sync.Once
			eng.SetStorageRootResolver(func(hash string) (string, error) {
				enteredOnce.Do(func() { close(entered) })
				select {
				case <-release:
					return payload.Root(hash), nil
				case <-eng.Context().Done():
					return "", eng.Context().Err()
				}
			})
			logs, err := logging.NewAsyncFiles(logging.FileOptions{Dir: filepath.Join(dir, "logs"), Console: io.Discard, Errors: io.Discard})
			if err != nil {
				t.Fatal(err)
			}
			defer logs.Close()
			_, _ = logs.Write([]byte("restore in progress\n"))
			ready := make(chan struct{})
			sw := usecase.NewSyncWorker(uc)
			defer sw.Close()
			router := deliveryhttp.NewRouterWithReadiness(uc, sw, logs, ready, http.FS(fstest.MapFS{"index.html": {Data: []byte("startup UI")}}))
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done, restored := make(chan error, 1), make(chan error, 1)
			go func() {
				done <- serve(ctx, &http.Server{Handler: router}, listener, func(ctx context.Context) {
					err := uc.RestoreTorrentsContext(ctx)
					restored <- err
					if err == nil {
						sw.Refresh()
						close(ready)
					}
					<-ctx.Done()
				}, func() { eng.BeginShutdown(); sw.Close() })
				close(done)
			}()
			defer func() { cancel(); <-done }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("restore did not reach persisted storage")
			}
			client := &http.Client{Timeout: time.Second}
			base := "http://" + listener.Addr().String()
			request := func(method, path string, want int) {
				t.Helper()
				req, _ := http.NewRequest(method, base+path, strings.NewReader(`{}`))
				req.Header.Set("Origin", "http://lampa.app")
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				body, _ := io.ReadAll(resp.Body)
				if resp.StatusCode != want || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
					t.Fatalf("%s %s: status=%d body=%s headers=%v", method, path, resp.StatusCode, body, resp.Header)
				}
				if want == http.StatusServiceUnavailable && (resp.Header.Get("Retry-After") != "1" || !strings.Contains(string(body), "restoring persisted torrents")) {
					t.Fatalf("missing startup response: %s %v", body, resp.Header)
				}
			}
			request("GET", "/echo", http.StatusOK)
			request("GET", "/", http.StatusOK)
			request("GET", "/api/v1/logs", http.StatusOK)
			request("GET", "/api/v1/logs/status", http.StatusOK)
			request("GET", "/api/v1/health/bt", http.StatusServiceUnavailable)
			request("GET", "/api/v1/torrents", http.StatusServiceUnavailable)
			request("POST", "/api/v1/torrent/add", http.StatusServiceUnavailable)
			request("POST", "/torrents", http.StatusServiceUnavailable)
			// The UI may subscribe before restore finishes. Keep the same SSE
			// connection through the first snapshot instead of rejecting EventSource.
			sseClient := &http.Client{Timeout: 10 * time.Second}
			sse, err := sseClient.Get(base + "/api/v1/events?view=compact")
			if err != nil {
				t.Fatal(err)
			}
			defer sse.Body.Close()
			if sse.StatusCode != http.StatusOK {
				t.Fatalf("startup SSE status: %d", sse.StatusCode)
			}
			events := bufio.NewReader(sse.Body)
			for _, want := range []string{": connected\n", "\n"} {
				if line, err := events.ReadString('\n'); err != nil || line != want {
					t.Fatalf("startup SSE: got %q, want %q, error %v", line, want, err)
				}
			}
			if finish {
				close(release)
				select {
				case err := <-restored:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("restore did not finish")
				}
				<-ready
				request("GET", "/api/v1/torrents", http.StatusOK)
				if line, err := events.ReadString('\n'); err != nil || !strings.HasPrefix(line, "id: ") {
					t.Fatalf("SSE snapshot version: %q, error %v", line, err)
				}
				if line, err := events.ReadString('\n'); err != nil || !strings.HasPrefix(line, "data: ") || !strings.Contains(line, hash) {
					t.Fatalf("SSE restored torrent: %q, error %v", line, err)
				}
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown did not cancel and join startup")
			}
			if _, err := io.ReadAll(events); err != nil {
				t.Fatalf("SSE did not close during shutdown: %v", err)
			}
			if !finish {
				if err := <-restored; !errors.Is(err, context.Canceled) {
					t.Fatalf("restore cancellation: %v", err)
				}
			}
		})
	}
}

func TestServeFailureCancelsStartup(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener.Close()
	stopped := make(chan struct{})
	err = serve(context.Background(), &http.Server{}, listener, func(ctx context.Context) {
		<-ctx.Done()
		close(stopped)
	}, func() {})
	if err == nil {
		t.Fatal("missing listener failure")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("server returned before startup retired")
	}
}
