package http

import (
	"net/http"

	"torrent-stream-hub/internal/delivery/http/api"
	"torrent-stream-hub/internal/delivery/http/torrserver"
	"torrent-stream-hub/internal/usecase"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)

func NewRouter(uc *usecase.TorrentUseCase, sw *usecase.SyncWorker) http.Handler {
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
		AllowCredentials: true,
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

	return r
}
