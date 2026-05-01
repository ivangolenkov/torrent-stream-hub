package torrserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"torrent-stream-hub/internal/delivery/http/response"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"
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
	r.Post("/torrent/upload", h.UploadTorrent)
	r.Post("/settings", h.Settings)
	r.Post("/viewed", h.Viewed)
	r.Get("/playlist", h.Playlist)
	r.Get("/stream", h.Stream)
	r.Get("/stream/{name}", h.Stream)
	r.Get("/play/{hash}/{id}", h.PlayAlias)
}

func (h *TorrServerHandler) Echo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("1.2.133"))
}

type TorrentsReq struct {
	Action string `json:"action"`
	Link   string `json:"link"`
	Hash   string `json:"hash"`
}

type torrentResponse struct {
	Hash       string              `json:"hash"`
	Title      string              `json:"title"`
	Name       string              `json:"name"`
	Size       int64               `json:"size"`
	Data       string              `json:"data"`
	Poster     string              `json:"poster"`
	FileStats  []torrentFileStat   `json:"file_stats,omitempty"`
	Downloaded int64               `json:"downloaded"`
	Progress   float64             `json:"progress"`
	Stat       models.TorrentState `json:"stat,omitempty"`
	Error      models.ErrorReason  `json:"error,omitempty"`
}

type torrentFileStat struct {
	ID     int    `json:"id"`
	Path   string `json:"path"`
	Length int64  `json:"length"`
	Size   int64  `json:"size"`
}

func (h *TorrServerHandler) Torrents(w http.ResponseWriter, r *http.Request) {
	var req TorrentsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logging.Debugf("torrserver torrents invalid JSON: %v", err)
		response.Error(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	logging.Infof("torrserver torrents action=%s link=%s", req.Action, logging.SafeMagnetSummary(req.Link))

	if req.Action == "list" {
		torrents, err := h.uc.GetAllTorrents()
		if err != nil {
			logging.Warnf("torrserver list failed: %v", err)
			response.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		logging.Debugf("torrserver list count=%d", len(torrents))
		items := make([]torrentResponse, 0, len(torrents))
		for _, t := range torrents {
			items = append(items, toTorrentResponse(t, false))
		}
		response.JSON(w, http.StatusOK, items)
		return
	}

	if req.Action == "get" {
		t, err := h.uc.GetTorrent(req.Hash)
		if err != nil || t == nil {
			logging.Debugf("torrserver get missing hash=%s err=%v", req.Hash, err)
			response.Error(w, http.StatusNotFound, "Torrent not found")
			return
		}
		response.JSON(w, http.StatusOK, toTorrentResponse(t, true))
		return
	}

	if req.Action == "add" {
		t, err := h.uc.AddMagnet(req.Link)
		if err != nil {
			logging.Warnf("torrserver add failed %s: %v", logging.SafeMagnetSummary(req.Link), err)
			response.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		response.JSON(w, http.StatusOK, toTorrentResponse(t, true))
		return
	}

	if req.Action == "drop" || req.Action == "rem" {
		if err := h.uc.Delete(req.Hash, req.Action == "rem"); err != nil {
			logging.Warnf("torrserver %s failed hash=%s: %v", req.Action, req.Hash, err)
			response.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		response.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	logging.Debugf("torrserver unknown action=%s", req.Action)
	response.Error(w, http.StatusBadRequest, "Unknown action")
}

func (h *TorrServerHandler) Settings(w http.ResponseWriter, r *http.Request) {
	// Stub for back-compatibility
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"CacheSize":209715200}`))
}

func (h *TorrServerHandler) UploadTorrent(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		logging.Debugf("torrent upload invalid multipart form: %v", err)
		response.Error(w, http.StatusBadRequest, "Invalid multipart form")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		logging.Debugf("torrent upload missing file: %v", err)
		response.Error(w, http.StatusBadRequest, "Torrent file is required")
		return
	}
	defer file.Close()

	filename := ""
	if header != nil {
		filename = header.Filename
	}
	logging.Infof("torrent upload filename=%q", filename)
	t, err := h.uc.AddTorrentFile(file)
	if err != nil {
		logging.Warnf("torrent upload failed filename=%q: %v", filename, err)
		response.Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	response.JSON(w, http.StatusAccepted, toTorrentResponse(t, true))
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
		logging.Debugf("playlist torrent not found hash=%s err=%v", hash, err)
		response.Error(w, http.StatusNotFound, "Torrent not found")
		return
	}
	logging.Debugf("playlist requested hash=%s files=%d", hash, len(t.Files))

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
	if hash == "" {
		hash = r.URL.Query().Get("link")
	}
	indexStr := r.URL.Query().Get("index")
	index, err := strconv.Atoi(indexStr)
	if err != nil {
		logging.Debugf("stream invalid index hash=%s index=%q", hash, indexStr)
		response.Error(w, http.StatusBadRequest, "Invalid index")
		return
	}

	if _, ok := r.URL.Query()["preload"]; ok {
		h.servePreloadStatus(w, r, hash, index)
		return
	}
	if _, ok := r.URL.Query()["stat"]; ok {
		h.servePreloadStatus(w, r, hash, index)
		return
	}

	h.serveStream(w, r, hash, index)
}

func toTorrentResponse(t *models.Torrent, includeFiles bool) torrentResponse {
	if t == nil {
		return torrentResponse{}
	}
	title := t.Name
	if title == "" {
		title = t.Hash
	}
	res := torrentResponse{
		Hash:       t.Hash,
		Title:      title,
		Name:       t.Name,
		Size:       t.Size,
		Data:       "{}",
		Poster:     "",
		Downloaded: t.Downloaded,
		Progress:   t.Progress,
		Stat:       t.State,
		Error:      t.Error,
	}
	if includeFiles {
		res.FileStats = toTorrentFileStats(t.Files)
	}
	return res
}

func toTorrentFileStats(files []*models.File) []torrentFileStat {
	stats := make([]torrentFileStat, 0, len(files))
	for _, f := range files {
		if f == nil {
			continue
		}
		stats = append(stats, torrentFileStat{
			ID:     f.Index,
			Path:   f.Path,
			Length: f.Size,
			Size:   f.Size,
		})
	}
	return stats
}

func (h *TorrServerHandler) servePreloadStatus(w http.ResponseWriter, r *http.Request, hash string, index int) {
	t, err := h.uc.GetTorrent(hash)
	if err != nil || t == nil {
		logging.Debugf("preload status torrent not found hash=%s file_index=%d err=%v", hash, index, err)
		response.Error(w, http.StatusNotFound, "Torrent not found")
		return
	}

	var size int64
	for _, f := range t.Files {
		if f != nil && f.Index == index {
			size = f.Size
			break
		}
	}
	if size == 0 {
		size = t.Size
	}

	response.JSON(w, http.StatusOK, map[string]any{
		"preloaded_bytes": size,
		"preload_size":    size,
		"download_speed":  t.DownloadSpeed,
	})
}

func (h *TorrServerHandler) PlayAlias(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	idStr := chi.URLParam(r, "id")
	index, err := strconv.Atoi(idStr)
	if err != nil {
		logging.Debugf("play alias invalid id hash=%s id=%q", hash, idStr)
		response.Error(w, http.StatusBadRequest, "Invalid id")
		return
	}

	h.serveStream(w, r, hash, index)
}

func (h *TorrServerHandler) serveStream(w http.ResponseWriter, r *http.Request, hash string, index int) {
	logging.Infof("stream request hash=%s file_index=%d range=%q remote=%s", hash, index, r.Header.Get("Range"), r.RemoteAddr)
	// 1. Get file from engine
	file, err := h.uc.GetTorrentFile(hash, index)
	if err != nil {
		status := http.StatusNotFound
		message := "File not found"
		if !errors.Is(err, usecase.ErrTorrentNotFound) {
			message = err.Error()
		}
		logging.Warnf("stream file lookup failed hash=%s file_index=%d status=%d: %v", hash, index, status, err)
		response.Error(w, status, message)
		return
	}

	// 2. Register stream reference counting with context
	// This will enable Sequential mode and automatically remove when request ends
	ctx := r.Context()
	err = h.uc.AddStream(ctx, hash, index)
	if err != nil {
		logging.Warnf("stream QoS registration failed hash=%s file_index=%d: %v", hash, index, err)
	}

	// 3. Set headers for streaming
	w.Header().Set("Accept-Ranges", "bytes")
	if contentType := streamContentType(file.DisplayPath()); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}

	// Create reader for the torrent file.
	// anacrolix/torrent provides an io.ReadSeeker which http.ServeContent uses natively to handle Range requests.
	reader := file.NewReader()
	reader.SetContext(ctx)
	defer reader.Close()
	reader.SetResponsive() // Better performance for streaming

	logging.Debugf("stream serving hash=%s file_index=%d file=%q size=%d", hash, index, file.DisplayPath(), file.Length())
	http.ServeContent(w, r, file.DisplayPath(), time.Time{}, reader)
}

func streamContentType(name string) string {
	contentType := mime.TypeByExtension(filepath.Ext(name))
	if contentType != "" {
		return contentType
	}

	return "application/octet-stream"
}
