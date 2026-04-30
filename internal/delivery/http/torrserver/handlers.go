package torrserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"torrent-stream-hub/internal/usecase"

	"github.com/go-chi/chi/v5"
)

type TorrServerHandler struct {
	uc *usecase.TorrentUseCase
}

func NewTorrServerHandler(uc *usecase.TorrentUseCase) *TorrServerHandler {
	return &TorrServerHandler{
		uc: uc,
	}
}

func (h *TorrServerHandler) RegisterRoutes(r chi.Router) {
	r.Get("/echo", h.Echo)
	r.Post("/torrents", h.Torrents)
	r.Post("/settings", h.Settings)
	r.Post("/viewed", h.Viewed)
	r.Get("/playlist", h.Playlist)
	r.Get("/stream", h.Stream)
	r.Get("/play/{hash}/{id}", h.PlayAlias)
}

func (h *TorrServerHandler) Echo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("1.2.133"))
}

type TorrentsReq struct {
	Action string `json:"action"`
	Link   string `json:"link"`
}

func (h *TorrServerHandler) Torrents(w http.ResponseWriter, r *http.Request) {
	var req TorrentsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	if req.Action == "list" {
		torrents, err := h.uc.GetAllTorrents()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(torrents)
		return
	}

	if req.Action == "add" {
		t, err := h.uc.AddMagnet(req.Link)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(t)
		return
	}

	http.Error(w, "Unknown action", http.StatusBadRequest)
}

func (h *TorrServerHandler) Settings(w http.ResponseWriter, r *http.Request) {
	// Stub for back-compatibility
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{}`))
}

func (h *TorrServerHandler) Viewed(w http.ResponseWriter, r *http.Request) {
	// Stub for back-compatibility
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

func (h *TorrServerHandler) Playlist(w http.ResponseWriter, r *http.Request) {
	hash := r.URL.Query().Get("hash")
	t, err := h.uc.GetTorrent(hash)
	if err != nil || t == nil {
		http.Error(w, "Torrent not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "audio/x-mpegurl")
	fmt.Fprintf(w, "#EXTM3U\n")

	for _, f := range t.Files {
		if f.IsMedia {
			fmt.Fprintf(w, "#EXTINF:-1,%s\n", f.Path)
			// Assuming server runs on the same host, ideally construct full URL
			host := r.Host
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			fmt.Fprintf(w, "%s://%s/play/%s/%d\n", scheme, host, t.Hash, f.Index)
		}
	}
}

func (h *TorrServerHandler) Stream(w http.ResponseWriter, r *http.Request) {
	hash := r.URL.Query().Get("hash")
	indexStr := r.URL.Query().Get("index")
	index, err := strconv.Atoi(indexStr)
	if err != nil {
		http.Error(w, "Invalid index", http.StatusBadRequest)
		return
	}

	h.serveStream(w, r, hash, index)
}

func (h *TorrServerHandler) PlayAlias(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	idStr := chi.URLParam(r, "id")
	index, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid id", http.StatusBadRequest)
		return
	}

	h.serveStream(w, r, hash, index)
}

func (h *TorrServerHandler) serveStream(w http.ResponseWriter, r *http.Request, hash string, index int) {
	// 1. Get file from engine
	file, err := h.uc.GetTorrentFile(hash, index)
	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	// 2. Register stream reference counting with context
	// This will enable Sequential mode and automatically remove when request ends
	ctx := r.Context()
	err = h.uc.AddStream(ctx, hash, index)
	if err != nil {
		// Log error, but still try to stream if possible
	}

	// 3. Set headers for streaming
	w.Header().Set("Accept-Ranges", "bytes")

	// Create reader for the torrent file.
	// anacrolix/torrent provides an io.ReadSeeker which http.ServeContent uses natively to handle Range requests.
	reader := file.NewReader()
	reader.SetResponsive() // Better performance for streaming

	http.ServeContent(w, r, file.DisplayPath(), time.Time{}, reader)
}
