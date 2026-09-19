package torrserver

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/go-chi/chi/v5"
)

func setupHTTPStreamServer(t *testing.T, pieceLength int64, name string, fileData []byte) (*httptest.Server, string, string, func()) {
	t.Helper()
	dir, _, h, cleanup := setupTorrServerIntegration(t)

	r := chi.NewRouter()
	h.RegisterRoutes(r)
	ts := httptest.NewServer(r)

	torrentBytes := createTorrserverTestTorrent(pieceLength, name, []metainfo.FileInfo{
		{Length: int64(len(fileData)), Path: []string{name + ".mp4"}},
	}, [][]byte{fileData})

	tor, err := h.uc.AddTorrentFile(bytes.NewReader(torrentBytes))
	if err != nil {
		ts.Close()
		cleanup()
		t.Fatalf("failed to add torrent file: %v", err)
	}
	hash := tor.Hash

	filePath := filepath.Join(dir, name, name+".mp4")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		ts.Close()
		cleanup()
		t.Fatalf("failed to create dir: %v", err)
	}
	if err := os.WriteFile(filePath, fileData, 0o644); err != nil {
		ts.Close()
		cleanup()
		t.Fatalf("failed to write file: %v", err)
	}

	// Verify pieces in engine
	tf, err := h.uc.GetTorrentFile(hash, 0)
	if err != nil {
		ts.Close()
		cleanup()
		t.Fatalf("failed to get torrent file: %v", err)
	}
	tRaw := tf.Torrent()
	for i := 0; i < tRaw.NumPieces(); i++ {
		_ = tRaw.Piece(i).VerifyData()
	}
	deadline := time.Now().Add(2 * time.Second)
	for i := 0; i < tRaw.NumPieces(); i++ {
		p := tRaw.Piece(i)
		for p.State().Marking && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
	}

	allCleanup := func() {
		ts.Close()
		cleanup()
	}

	return ts, hash, dir, allCleanup
}

func TestHTTPStream_DiskSource_RangeRequests(t *testing.T) {
	const dataSize = 4096
	fileData := make([]byte, dataSize)
	for i := range fileData {
		fileData[i] = byte(i % 251)
	}

	ts, hash, _, cleanup := setupHTTPStreamServer(t, 1024, "test_ranges", fileData)
	defer cleanup()

	client := ts.Client()
	streamURL := fmt.Sprintf("%s/stream?link=%s&index=1", ts.URL, hash)

	// 1. GET without Range: 200 OK, full body
	t.Run("GET without Range", func(t *testing.T) {
		resp, err := client.Get(streamURL)
		if err != nil {
			t.Fatalf("GET failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected status 200, got %d", resp.StatusCode)
		}
		if resp.Header.Get("Accept-Ranges") != "bytes" {
			t.Fatalf("expected Accept-Ranges: bytes, got %q", resp.Header.Get("Accept-Ranges"))
		}
		if resp.Header.Get("Content-Disposition") != "" {
			t.Fatalf("expected no Content-Disposition on stream, got %q", resp.Header.Get("Content-Disposition"))
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("failed to read body: %v", err)
		}
		if !bytes.Equal(body, fileData) {
			t.Fatalf("body mismatch: got %d bytes, want %d", len(body), len(fileData))
		}
	})

	// 2. GET Range bytes=0-
	t.Run("GET bytes=0-", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, streamURL, nil)
		req.Header.Set("Range", "bytes=0-")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("expected status 206, got %d", resp.StatusCode)
		}
		if cr := resp.Header.Get("Content-Range"); cr != fmt.Sprintf("bytes 0-%d/%d", dataSize-1, dataSize) {
			t.Fatalf("expected Content-Range bytes 0-%d/%d, got %q", dataSize-1, dataSize, cr)
		}
		body, _ := io.ReadAll(resp.Body)
		if !bytes.Equal(body, fileData) {
			t.Fatalf("body mismatch for bytes=0-")
		}
	})

	// 3. GET Range bytes=100-199
	t.Run("GET bytes=100-199", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, streamURL, nil)
		req.Header.Set("Range", "bytes=100-199")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("expected status 206, got %d", resp.StatusCode)
		}
		if cr := resp.Header.Get("Content-Range"); cr != fmt.Sprintf("bytes 100-199/%d", dataSize) {
			t.Fatalf("expected Content-Range bytes 100-199/%d, got %q", dataSize, cr)
		}
		body, _ := io.ReadAll(resp.Body)
		if !bytes.Equal(body, fileData[100:200]) {
			t.Fatalf("body mismatch for bytes=100-199")
		}
	})

	// 4. GET Range bytes=100-
	t.Run("GET bytes=100-", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, streamURL, nil)
		req.Header.Set("Range", "bytes=100-")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("expected status 206, got %d", resp.StatusCode)
		}
		if cr := resp.Header.Get("Content-Range"); cr != fmt.Sprintf("bytes 100-%d/%d", dataSize-1, dataSize) {
			t.Fatalf("expected Content-Range bytes 100-%d/%d, got %q", dataSize-1, dataSize, cr)
		}
		body, _ := io.ReadAll(resp.Body)
		if !bytes.Equal(body, fileData[100:]) {
			t.Fatalf("body mismatch for bytes=100-")
		}
	})

	// 5. GET Suffix Range bytes=-200
	t.Run("GET suffix bytes=-200", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, streamURL, nil)
		req.Header.Set("Range", "bytes=-200")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("expected status 206, got %d", resp.StatusCode)
		}
		expectedCR := fmt.Sprintf("bytes %d-%d/%d", dataSize-200, dataSize-1, dataSize)
		if cr := resp.Header.Get("Content-Range"); cr != expectedCR {
			t.Fatalf("expected Content-Range %s, got %q", expectedCR, cr)
		}
		body, _ := io.ReadAll(resp.Body)
		if !bytes.Equal(body, fileData[dataSize-200:]) {
			t.Fatalf("body mismatch for suffix range")
		}
	})

	// 6. GET bytes=0-0
	t.Run("GET bytes=0-0", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, streamURL, nil)
		req.Header.Set("Range", "bytes=0-0")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("expected status 206, got %d", resp.StatusCode)
		}
		if cr := resp.Header.Get("Content-Range"); cr != fmt.Sprintf("bytes 0-0/%d", dataSize) {
			t.Fatalf("expected Content-Range bytes 0-0/%d, got %q", dataSize, cr)
		}
		body, _ := io.ReadAll(resp.Body)
		if len(body) != 1 || body[0] != fileData[0] {
			t.Fatalf("body mismatch for bytes=0-0")
		}
	})

	// 7. GET Out of bounds Range -> 416
	t.Run("GET out of bounds bytes=5000-6000", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, streamURL, nil)
		req.Header.Set("Range", "bytes=5000-6000")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("expected status 416, got %d", resp.StatusCode)
		}
		if cr := resp.Header.Get("Content-Range"); cr != fmt.Sprintf("bytes */%d", dataSize) {
			t.Fatalf("expected Content-Range bytes */%d, got %q", dataSize, cr)
		}
	})

	// 8. GET multi-range -> multipart/byteranges
	t.Run("GET multi-range bytes=0-10, 20-30", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, streamURL, nil)
		req.Header.Set("Range", "bytes=0-10, 20-30")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("expected status 206, got %d", resp.StatusCode)
		}
		ct := resp.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "multipart/byteranges") {
			t.Fatalf("expected multipart/byteranges content type, got %q", ct)
		}
	})
}

func TestHTTPStream_DiskSource_HEADRequest(t *testing.T) {
	fileData := bytes.Repeat([]byte("H"), 2048)
	ts, hash, _, cleanup := setupHTTPStreamServer(t, 1024, "test_head_http", fileData)
	defer cleanup()

	client := ts.Client()
	streamURL := fmt.Sprintf("%s/stream?link=%s&index=1", ts.URL, hash)

	// HEAD without Range
	resp, err := client.Head(streamURL)
	if err != nil {
		t.Fatalf("HEAD failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
	if resp.ContentLength != 2048 {
		t.Fatalf("expected Content-Length 2048, got %d", resp.ContentLength)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Fatalf("expected empty body for HEAD, got %d bytes", len(body))
	}

	// HEAD with Range
	req, _ := http.NewRequest(http.MethodHead, streamURL, nil)
	req.Header.Set("Range", "bytes=100-199")
	respRange, err := client.Do(req)
	if err != nil {
		t.Fatalf("HEAD with Range failed: %v", err)
	}
	defer respRange.Body.Close()

	if respRange.StatusCode != http.StatusPartialContent {
		t.Fatalf("expected status 206 for HEAD with Range, got %d", respRange.StatusCode)
	}
	if cr := respRange.Header.Get("Content-Range"); cr != "bytes 100-199/2048" {
		t.Fatalf("expected Content-Range bytes 100-199/2048, got %q", cr)
	}
	bodyRange, _ := io.ReadAll(respRange.Body)
	if len(bodyRange) != 0 {
		t.Fatalf("expected empty body for HEAD with Range, got %d bytes", len(bodyRange))
	}
}

func TestHTTPStream_StreamingAliasesAndHeaders(t *testing.T) {
	fileData := bytes.Repeat([]byte("A"), 2048)
	ts, hash, _, cleanup := setupHTTPStreamServer(t, 1024, "test_aliases", fileData)
	defer cleanup()

	client := ts.Client()

	aliases := []string{
		fmt.Sprintf("%s/stream?link=%s&index=1", ts.URL, hash),
		fmt.Sprintf("%s/stream/video.mp4?link=%s&index=1", ts.URL, hash),
		fmt.Sprintf("%s/play/%s/1", ts.URL, hash),
	}

	for _, url := range aliases {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("getcontentFeatures.dlna.org", "1")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET alias %s failed: %v", url, err)
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("alias %s: expected status 200, got %d", url, resp.StatusCode)
		}
		if resp.Header.Get("transferMode.dlna.org") != "Streaming" {
			resp.Body.Close()
			t.Fatalf("alias %s: expected DLNA transferMode", url)
		}
		if !strings.Contains(resp.Header.Get("contentFeatures.dlna.org"), "DLNA.ORG") {
			resp.Body.Close()
			t.Fatalf("alias %s: expected DLNA features header", url)
		}
		if resp.Header.Get("Content-Disposition") != "" {
			resp.Body.Close()
			t.Fatalf("alias %s: unexpected Content-Disposition header", url)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("alias %s: failed to read body: %v", url, err)
		}
		if !bytes.Equal(body, fileData) {
			t.Fatalf("alias %s: body does not match expected data", url)
		}
	}
}
