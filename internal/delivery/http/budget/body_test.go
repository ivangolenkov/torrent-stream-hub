package budget

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestControlBodyLimitIgnoresContentLengthClaims(t *testing.T) {
	limits := Defaults()
	limits.Control = 8
	h := Bodies(limits)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("oversized body reached handler") }))
	for _, length := range []int64{-1, 1, 20} {
		r := httptest.NewRequest("POST", "/cache", bytes.NewBufferString("123456789"))
		r.ContentLength = length
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 413 {
			t.Fatal(length, w.Code, w.Body.String())
		}
	}
}

func TestSlowBodyDeadlineReturns408(t *testing.T) {
	limits := Defaults()
	limits.ReadTimeout = 30 * time.Millisecond
	server := httptest.NewServer(Bodies(limits)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("slow body reached handler") })))
	defer server.Close()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprint(conn, "POST /cache HTTP/1.1\r\nHost: localhost\r\nContent-Length: 20\r\n\r\nx")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 408 {
		t.Fatal(resp.Status)
	}
}

func uploadRequest(t *testing.T, data string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", "x.torrent")
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(file, data)
	writer.Close()
	r := httptest.NewRequest("POST", "/torrent/upload", &body)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	return r
}

func TestUploadBudgetCleanupFailureAndAdmission(t *testing.T) {
	b := NewJobs(t.TempDir())
	b.Limits.Torrent = 8
	b.Limits.Envelope = 1024
	for _, data := range []string{"12345678", "123456789"} {
		file, _, cleanup, err := b.ReadUpload(httptest.NewRecorder(), uploadRequest(t, data))
		if len(data) > 8 {
			if Status(err) != 413 {
				t.Fatal(err)
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(file)
			if string(got) != data {
				t.Fatal(string(got))
			}
		}
		cleanup()
		entries, _ := os.ReadDir(b.Dir)
		if len(entries) != 0 {
			t.Fatal("upload spill leaked", entries)
		}
	}
	a, _ := b.Acquire(context.Background())
	c, _ := b.Acquire(context.Background())
	if _, err := b.Acquire(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatal("unbounded jobs", err)
	}
	a()
	a()
	c()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, cleanup, err := b.ReadUpload(httptest.NewRecorder(), uploadRequest(t, "x").WithContext(ctx)); !errors.Is(err, context.Canceled) {
		cleanup()
		t.Fatal(err)
	}
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	b.Dir = blocked
	_, _, cleanup, err := b.ReadUpload(httptest.NewRecorder(), uploadRequest(t, "x"))
	defer cleanup()
	if Status(err) != 507 {
		t.Fatal("temp storage error hidden", err, Status(err))
	}
}

func TestControlBudgetIncludesBlockedHandlers(t *testing.T) {
	entered := make(chan struct{}, 32)
	release := make(chan struct{})
	var workers sync.WaitGroup
	h := Bodies(Defaults())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { entered <- struct{}{}; <-release }))
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/cache", bytes.NewBufferString("{}")))
		}()
	}
	defer workers.Wait()
	defer close(release)
	for i := 0; i < 32; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("admitted handler did not start")
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/cache", bytes.NewBufferString("{}")))
	if w.Code != 429 {
		t.Fatal("blocked parsed commands retain unbounded bodies", w.Code)
	}
}

func TestMalformedAndInterruptedUploadRemovesWrittenSpill(t *testing.T) {
	for _, cut := range []int{5, 60} {
		t.Run(fmt.Sprint(cut), func(t *testing.T) {
			b := NewJobs(t.TempDir())
			request := uploadRequest(t, strings.Repeat("data", 2000))
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			request.Body = io.NopCloser(bytes.NewReader(body[:len(body)-cut]))
			_, _, cleanup, err := b.ReadUpload(httptest.NewRecorder(), request)
			defer cleanup()
			if err == nil {
				t.Fatal("accepted incomplete multipart")
			}
			entries, err := os.ReadDir(b.Dir)
			if err != nil || len(entries) != 0 {
				t.Fatal("incomplete upload leaked spill", entries, err)
			}
		})
	}
}
