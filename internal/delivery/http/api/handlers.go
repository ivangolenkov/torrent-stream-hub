package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"torrent-stream-hub/internal/delivery/http/response"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"
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
	r.Put("/torrent/{hash}/priority", h.SetTorrentPriority)
	r.Put("/torrent/{hash}/file/{index}/priority", h.SetFilePriority)
	r.Get("/torrent/{hash}/files", h.GetTorrentFiles)
	r.Get("/torrent/{hash}/download", h.DownloadTorrent)
	r.Head("/torrent/{hash}/download", h.DownloadTorrent)
	r.Get("/torrent/{hash}/file/{index}/download", h.DownloadTorrentFile)
	r.Head("/torrent/{hash}/file/{index}/download", h.DownloadTorrentFile)
	r.Get("/torrent/{hash}/pieces", h.GetTorrentPieces)
	r.Get("/health/bt", h.BTHealth)
	r.Get("/events", h.SSEEvents)
}

func (h *APIHandler) GetAllTorrents(w http.ResponseWriter, r *http.Request) {
	torrents, err := h.uc.GetAllTorrents()
	if err != nil {
		logging.Warnf("api get torrents failed: %v", err)
		response.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	logging.Debugf("api get torrents count=%d", len(torrents))
	response.JSON(w, http.StatusOK, torrents)
}

type AddTorrentReq struct {
	Link       string `json:"link"`
	SavePath   string `json:"save_path"`
	Sequential bool   `json:"sequential"`
	Poster     string `json:"poster"`
}

func (h *APIHandler) AddTorrent(w http.ResponseWriter, r *http.Request) {
	var req AddTorrentReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logging.Debugf("api add torrent invalid JSON: %v", err)
		response.Error(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if req.Link == "" {
		logging.Debugf("api add torrent missing link")
		response.Error(w, http.StatusBadRequest, "Link is required")
		return
	}

	logging.Infof("api add torrent %s", logging.SafeMagnetSummary(req.Link))
	t, err := h.uc.AddMagnetWithMetadata(req.Link, usecase.TorrentMetadata{Poster: req.Poster})
	if err != nil {
		logging.Warnf("api add torrent failed %s: %v", logging.SafeMagnetSummary(req.Link), err)
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
		logging.Debugf("api torrent action invalid JSON hash=%s: %v", hash, err)
		response.Error(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	logging.Infof("api torrent action hash=%s action=%s delete_files=%t", hash, req.Action, req.DeleteFiles)
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
		logging.Debugf("api torrent action unknown hash=%s action=%s", hash, req.Action)
		response.Error(w, http.StatusBadRequest, "Unknown action")
		return
	}

	if err != nil {
		if errors.Is(err, usecase.ErrTorrentNotFound) {
			logging.Debugf("api torrent action not found hash=%s action=%s", hash, req.Action)
			response.Error(w, http.StatusNotFound, err.Error())
			return
		}
		logging.Warnf("api torrent action failed hash=%s action=%s: %v", hash, req.Action, err)
		response.Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	response.Status(w, http.StatusOK, "ok")
}

type PriorityReq struct {
	Priority int `json:"priority"`
}

func (h *APIHandler) SetTorrentPriority(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	var req PriorityReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	err := h.uc.SetTorrentFilesPriority(hash, models.FilePriority(req.Priority))
	if err != nil {
		if errors.Is(err, usecase.ErrTorrentNotFound) {
			response.Error(w, http.StatusNotFound, "Torrent not found")
			return
		}
		response.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	response.Status(w, http.StatusOK, "ok")
}

func (h *APIHandler) SetFilePriority(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	indexStr := chi.URLParam(r, "index")

	index, err := strconv.Atoi(indexStr)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "Invalid file index")
		return
	}

	var req PriorityReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	err = h.uc.SetFilePriority(hash, index, models.FilePriority(req.Priority))
	if err != nil {
		if errors.Is(err, usecase.ErrTorrentNotFound) {
			response.Error(w, http.StatusNotFound, "Torrent not found")
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
		logging.Warnf("api get torrent files failed hash=%s: %v", hash, err)
		response.Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	if t == nil {
		logging.Debugf("api get torrent files not found hash=%s", hash)
		response.Error(w, http.StatusNotFound, "Torrent not found")
		return
	}

	logging.Debugf("api get torrent files hash=%s files=%d", hash, len(t.Files))
	response.JSON(w, http.StatusOK, t.Files)
}

func (h *APIHandler) GetTorrentPieces(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	pieces, err := h.uc.GetTorrentPieces(hash)
	if err != nil {
		if errors.Is(err, usecase.ErrTorrentNotFound) {
			response.Error(w, http.StatusNotFound, "Torrent not found")
			return
		}
		logging.Warnf("api get torrent pieces failed hash=%s: %v", hash, err)
		response.Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(pieces))
}

func (h *APIHandler) BTHealth(w http.ResponseWriter, r *http.Request) {
	response.JSON(w, http.StatusOK, h.uc.BTHealth())
}

// SSEEvents handles Server-Sent Events connection for UI updates
func (h *APIHandler) SSEEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan []byte, 10)
	h.syncWorker.AddClient(ch)
	defer h.syncWorker.RemoveClient(ch)
	logging.Infof("api sse client connected remote=%s", r.RemoteAddr)

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
			logging.Infof("api sse client disconnected remote=%s", r.RemoteAddr)
			return
		}
	}
}
