package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"torrent-stream-hub/internal/delivery/http/response"
	"torrent-stream-hub/internal/usecase"

	"github.com/go-chi/chi/v5"
)

type APIHandler struct {
	uc         *usecase.TorrentUseCase
	syncWorker *usecase.SyncWorker
}

func NewAPIHandler(uc *usecase.TorrentUseCase, sw *usecase.SyncWorker) *APIHandler {
	return &APIHandler{
		uc:         uc,
		syncWorker: sw,
	}
}

func (h *APIHandler) RegisterRoutes(r chi.Router) {
	r.Get("/torrents", h.GetAllTorrents)
	r.Post("/torrent/add", h.AddTorrent)
	r.Post("/torrent/{hash}/action", h.TorrentAction)
	r.Get("/torrent/{hash}/files", h.GetTorrentFiles)
	r.Get("/events", h.SSEEvents)
}

func (h *APIHandler) GetAllTorrents(w http.ResponseWriter, r *http.Request) {
	torrents, err := h.uc.GetAllTorrents()
	if err != nil {
		response.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	response.JSON(w, http.StatusOK, torrents)
}

type AddTorrentReq struct {
	Link       string `json:"link"`
	SavePath   string `json:"save_path"`
	Sequential bool   `json:"sequential"`
}

func (h *APIHandler) AddTorrent(w http.ResponseWriter, r *http.Request) {
	var req AddTorrentReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if req.Link == "" {
		response.Error(w, http.StatusBadRequest, "Link is required")
		return
	}

	t, err := h.uc.AddMagnet(req.Link)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	response.JSON(w, http.StatusAccepted, t)
}

type ActionReq struct {
	Action      string `json:"action"`
	DeleteFiles bool   `json:"delete_files"`
}

func (h *APIHandler) TorrentAction(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	var req ActionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	var err error
	switch req.Action {
	case "pause":
		err = h.uc.Pause(hash)
	case "resume":
		err = h.uc.Resume(hash)
	case "delete":
		err = h.uc.Delete(hash, req.DeleteFiles)
	case "recheck":
		// TODO: implement recheck
	default:
		response.Error(w, http.StatusBadRequest, "Unknown action")
		return
	}

	if err != nil {
		if errors.Is(err, usecase.ErrTorrentNotFound) {
			response.Error(w, http.StatusNotFound, err.Error())
			return
		}
		response.Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	response.Status(w, http.StatusOK, "ok")
}

func (h *APIHandler) GetTorrentFiles(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	t, err := h.uc.GetTorrent(hash)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	if t == nil {
		response.Error(w, http.StatusNotFound, "Torrent not found")
		return
	}

	response.JSON(w, http.StatusOK, t.Files)
}

// SSEEvents handles Server-Sent Events connection for UI updates
func (h *APIHandler) SSEEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan []byte, 10)
	h.syncWorker.AddClient(ch)
	defer h.syncWorker.RemoveClient(ch)

	for {
		select {
		case data, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}
