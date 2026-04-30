package http

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"torrent-stream-hub/internal/delivery/http/api"
	"torrent-stream-hub/internal/delivery/http/torrserver"
	"torrent-stream-hub/internal/usecase"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)

func NewRouter(uc *usecase.TorrentUseCase, sw *usecase.SyncWorker, staticFS ...http.FileSystem) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Configure CORS to allow all origins as per requirements for Smart TV/Local network
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	// TorrServer Layer
	torrServerHandler := torrserver.NewTorrServerHandler(uc)
	r.Group(func(r chi.Router) {
		torrServerHandler.RegisterRoutes(r)
	})

	// Management REST API for Web GUI
	apiHandler := api.NewAPIHandler(uc, sw)
	r.Route("/api/v1", func(r chi.Router) {
		apiHandler.RegisterRoutes(r)
	})

	if len(staticFS) > 0 && staticFS[0] != nil {
		fileServer := http.FileServer(staticFS[0])
		r.Get("/*", spaFallback(staticFS[0], fileServer))
		r.NotFound(spaFallback(staticFS[0], fileServer))
	}

	return r
}

func spaFallback(staticFS http.FileSystem, fileServer http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/stream") || strings.HasPrefix(r.URL.Path, "/play/") {
			http.NotFound(w, r)
			return
		}

		cleanPath := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if cleanPath == "." {
			cleanPath = "index.html"
		}

		file, err := staticFS.Open(cleanPath)
		if err == nil {
			defer file.Close()
			if stat, statErr := file.Stat(); statErr == nil && !stat.IsDir() {
				fileServer.ServeHTTP(w, r)
				return
			}
		}

		index, err := staticFS.Open("index.html")
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer index.Close()

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if _, err := io.Copy(w, index); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}
