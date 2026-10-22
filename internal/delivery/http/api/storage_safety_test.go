package api

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"torrent-stream-hub/internal/models"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/go-chi/chi/v5"
)

func TestDownloadSafetyAndHEAD(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"valid", 200}, {"missing", 404}, {"symlink", 409}, {"directory", 409},
		{"size", 409}, {"part", 409}, {"unknown-layout", 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uc, repo, dir, cleanup := setupAPITestUseCase(t)
			defer cleanup()
			hash := writeDownloadMetainfo(t, dir, "safe.txt", []metainfo.FileInfo{{Length: 4}}, []byte("safe"))
			if err := repo.SaveTorrent(&models.Torrent{Hash: hash, Name: "safe.txt", Size: 4, Downloaded: 4, State: models.StatePaused, Files: []*models.File{{Index: 0, Path: "safe.txt", Size: 4, Downloaded: 4}}}); err != nil {
				t.Fatal(err)
			}
			full := filepath.Join(dir, "safe.txt")
			if tc.name != "unknown-layout" {
				if err := os.WriteFile(full, []byte("safe"), 0600); err != nil {
					t.Fatal(err)
				}
				dir = activateDownloadFixture(t, uc, repo, dir, hash)
				full = filepath.Join(dir, "safe.txt")
			}
			switch tc.name {
			case "missing":
				if err := os.Remove(full); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(full); err != nil {
					t.Fatal(err)
				}
				external := filepath.Join(t.TempDir(), "secret")
				if err := os.WriteFile(external, []byte("evil"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, full); err != nil {
					t.Skip(err)
				}
			case "directory":
				if err := os.Remove(full); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(full, 0700); err != nil {
					t.Fatal(err)
				}
			case "size":
				if err := os.Truncate(full, 1); err != nil {
					t.Fatal(err)
				}
			case "part":
				if err := os.WriteFile(full+".part", nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "unknown-layout":
				if err := os.Remove(filepath.Join(dir, "metainfo", hash+".torrent")); err != nil {
					t.Fatal(err)
				}
			}
			router := chi.NewRouter()
			NewAPIHandler(uc, nil).RegisterRoutes(router)
			for _, suffix := range []string{"/file/0/download", "/download"} {
				for _, method := range []string{http.MethodGet, http.MethodHead} {
					rr := httptest.NewRecorder()
					router.ServeHTTP(rr, httptest.NewRequest(method, "/torrent/"+hash+suffix, nil))
					if rr.Code != tc.status {
						t.Fatalf("%s %s: %d %s", method, suffix, rr.Code, rr.Body.String())
					}
					if tc.status == 200 && method == http.MethodHead && rr.Body.Len() != 0 {
						t.Fatal("HEAD transferred data")
					}
					if strings.Contains(rr.Body.String(), dir) {
						t.Fatal("absolute storage path leaked")
					}
				}
			}
		})
	}
}

type mutateZIPWriter struct {
	*httptest.ResponseRecorder
	mutate func()
}

func (w *mutateZIPWriter) Write(p []byte) (int, error) {
	if w.mutate != nil {
		f := w.mutate
		w.mutate = nil
		f()
	}
	return w.ResponseRecorder.Write(p)
}

func TestZIPReopensEntriesSafelyAfterHeaders(t *testing.T) {
	uc, repo, dir, cleanup := setupAPITestUseCase(t)
	defer cleanup()
	size := int64(8192) // Exceed the ZIP writer buffer, ensuring mutation before entry two.
	hash := writeDownloadMetainfo(t, dir, "archive", []metainfo.FileInfo{{Length: size, Path: []string{"a"}}, {Length: size, Path: []string{"b"}}})
	if err := os.Mkdir(filepath.Join(dir, "archive"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(dir, "archive", name), make([]byte, size), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.SaveTorrent(&models.Torrent{Hash: hash, Name: "archive", Size: 2 * size, Downloaded: 2 * size, State: models.StatePaused, Files: []*models.File{{Index: 0, Path: "a", Size: size, Downloaded: size}, {Index: 1, Path: "b", Size: size, Downloaded: size}}}); err != nil {
		t.Fatal(err)
	}
	dir = activateDownloadFixture(t, uc, repo, dir, hash)
	external := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(external, bytes.Repeat([]byte("s"), int(size)), 0600); err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	NewAPIHandler(uc, nil).RegisterRoutes(router)
	w := &mutateZIPWriter{ResponseRecorder: httptest.NewRecorder(), mutate: func() {
		if err := os.Remove(filepath.Join(dir, "archive", "b")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, filepath.Join(dir, "archive", "b")); err != nil {
			t.Fatal(err)
		}
	}}
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/torrent/"+hash+"/download", nil))
	if w.mutate != nil {
		t.Fatal("mutation was not exercised")
	}
	if bytes.Contains(w.Body.Bytes(), bytes.Repeat([]byte("s"), 64)) {
		t.Fatal("ZIP leaked external content")
	}
	if _, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len())); err == nil {
		t.Fatal("failed transfer looks like a successfully completed ZIP")
	}
}
