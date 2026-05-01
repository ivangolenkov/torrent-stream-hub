package torrserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"torrent-stream-hub/internal/delivery/http/response"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/usecase"

	"github.com/go-chi/chi/v5"
)

type TorrServerHandler struct {
	uc       *usecase.TorrentUseCase
	preloads map[preloadKey]preloadState
	mu       sync.Mutex
}

const defaultPreloadSize int64 = 20 << 20

type preloadKey struct {
	Hash  string
	Index int
}

type preloadState struct {
	TargetBytes int64
	ReadBytes   int64
	StartedAt   time.Time
}

func NewTorrServerHandler(uc *usecase.TorrentUseCase) *TorrServerHandler {
	return &TorrServerHandler{
		uc:       uc,
		preloads: make(map[preloadKey]preloadState),
	}
}

func (h *TorrServerHandler) RegisterRoutes(r chi.Router) {
	r.Get("/echo", h.Echo)
	r.Post("/torrents", h.Torrents)
	r.Post("/torrent/upload", h.UploadTorrent)
	r.Post("/settings", h.Settings)
	r.Post("/cache", h.Cache)
	r.Post("/viewed", h.Viewed)
	r.Get("/playlist", h.Playlist)
	r.Get("/stream", h.Stream)
	r.Head("/stream", h.Stream)
	r.Get("/stream/{name}", h.Stream)
	r.Head("/stream/{name}", h.Stream)
	r.Get("/play/{hash}/{id}", h.PlayAlias)
	r.Head("/play/{hash}/{id}", h.PlayAlias)
}

func (h *TorrServerHandler) Echo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("1.2.133"))
}

type TorrentsReq struct {
	Action   string `json:"action"`
	Link     string `json:"link"`
	Hash     string `json:"hash"`
	Title    string `json:"title"`
	Data     string `json:"data"`
	Poster   string `json:"poster"`
	Category string `json:"category"`
	SaveToDB bool   `json:"save_to_db"`
}

type torrentResponse struct {
	Hash             string              `json:"hash"`
	Title            string              `json:"title"`
	Name             string              `json:"name"`
	Size             int64               `json:"size"`
	Data             string              `json:"data"`
	Poster           string              `json:"poster"`
	Category         string              `json:"category"`
	Timestamp        int64               `json:"timestamp"`
	StatString       string              `json:"stat_string,omitempty"`
	LoadedSize       int64               `json:"loaded_size"`
	TorrentSize      int64               `json:"torrent_size"`
	PreloadedBytes   int64               `json:"preloaded_bytes"`
	PreloadSize      int64               `json:"preload_size"`
	DownloadSpeed    int64               `json:"download_speed"`
	UploadSpeed      int64               `json:"upload_speed"`
	TotalPeers       int                 `json:"total_peers"`
	PendingPeers     int                 `json:"pending_peers"`
	ActivePeers      int                 `json:"active_peers"`
	ConnectedSeeders int                 `json:"connected_seeders"`
	HalfOpenPeers    int                 `json:"half_open_peers"`
	FileStats        []torrentFileStat   `json:"file_stats,omitempty"`
	Downloaded       int64               `json:"downloaded"`
	Progress         float64             `json:"progress"`
	Stat             models.TorrentState `json:"stat,omitempty"`
	Error            models.ErrorReason  `json:"error,omitempty"`
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
		t, err := h.addTorrent(req)
		if err != nil {
			logging.Warnf("torrserver add failed %s: %v", logging.SafeMagnetSummary(req.Link), err)
			response.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		response.JSON(w, http.StatusOK, toTorrentResponse(t, true))
		return
	}

	if req.Action == "set" {
		t, err := h.uc.UpdateMetadata(req.Hash, usecase.TorrentMetadata{
			Title: req.Title, Data: req.Data, Poster: req.Poster, Category: req.Category,
		})
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, usecase.ErrTorrentNotFound) {
				status = http.StatusNotFound
			}
			logging.Warnf("torrserver set failed hash=%s: %v", req.Hash, err)
			response.Error(w, status, err.Error())
			return
		}
		response.JSON(w, http.StatusOK, toTorrentResponse(t, true))
		return
	}

	if req.Action == "drop" {
		// Lampa calls drop when closing a torrent page. Keep engine and DB in sync by making it a safe no-op.
		response.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	if req.Action == "rem" {
		if err := h.uc.Delete(req.Hash, false); err != nil {
			logging.Warnf("torrserver rem failed hash=%s: %v", req.Hash, err)
			response.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		response.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	if req.Action == "wipe" {
		// Wipe is too destructive for the compatibility layer. Accept it but do nothing.
		response.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	logging.Debugf("torrserver unknown action=%s", req.Action)
	response.Error(w, http.StatusBadRequest, "Unknown action")
}

func (h *TorrServerHandler) addTorrent(req TorrentsReq) (*models.Torrent, error) {
	metadata := usecase.TorrentMetadata{
		Title:    req.Title,
		Data:     req.Data,
		Poster:   req.Poster,
		Category: req.Category,
	}
	link := normalizeTorrentLink(req.Link)
	if strings.HasPrefix(strings.ToLower(link), "http://") || strings.HasPrefix(strings.ToLower(link), "https://") {
		resolved, torrentData, err := resolveHTTPLink(link)
		if err != nil {
			return nil, err
		}
		if len(torrentData) > 0 {
			t, err := h.uc.AddTorrentFile(bytes.NewReader(torrentData))
			if err != nil {
				return nil, err
			}
			return h.uc.UpdateMetadata(t.Hash, metadata)
		}
		link = normalizeTorrentLink(resolved)
	}
	return h.uc.AddMagnetWithMetadata(link, metadata)
}

func (h *TorrServerHandler) Settings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Action == "" || req.Action == "get" {
		response.JSON(w, http.StatusOK, map[string]any{
			"CacheSize":                209715200,
			"ReaderReadAHead":          20971520,
			"PreloadCache":             20971520,
			"TorrentDisconnectTimeout": 30,
			"ResponsiveMode":           true,
		})
		return
	}
	if req.Action == "set" || req.Action == "def" {
		response.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	response.Error(w, http.StatusBadRequest, "Unknown action")
}

func (h *TorrServerHandler) Cache(w http.ResponseWriter, r *http.Request) {
	req, err := decodeCacheRequest(r)
	if err != nil {
		logging.Debugf("torrserver cache invalid JSON: %v", err)
		response.Error(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	if req.Action != "get" {
		response.JSON(w, http.StatusOK, map[string]any{})
		return
	}
	hash := firstString(req.Hash, req.Link)
	index := torrserverIndexToInternal(req.Index)
	status, err := h.uc.GetCacheStatus(hash, index, req.Offset)
	if err != nil {
		logging.Debugf("torrserver cache unavailable hash=%s index=%d: %v", hash, index, err)
		response.JSON(w, http.StatusOK, map[string]any{})
		return
	}
	filled := status.VirtualCacheBytes
	t, _ := h.uc.GetTorrent(hash)
	response.JSON(w, http.StatusOK, map[string]any{
		"Hash":         hash,
		"Capacity":     defaultPreloadSize,
		"Filled":       filled,
		"PiecesLength": 0,
		"PiecesCount":  0,
		"Torrent":      toTorrentResponse(t, true),
		"Pieces":       map[int]any{},
		"Readers":      []any{},
	})
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
	if poster := strings.TrimSpace(r.FormValue("poster")); poster != "" {
		t, err = h.uc.UpdateMetadata(t.Hash, usecase.TorrentMetadata{Poster: poster})
		if err != nil {
			logging.Warnf("torrent upload metadata failed filename=%q hash=%s: %v", filename, t.Hash, err)
			response.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
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
			fmt.Fprintf(w, "%s://%s/play/%s/%d\n", scheme, host, t.Hash, internalIndexToTorrserver(f.Index))
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
	if _, usingLink := r.URL.Query()["link"]; usingLink {
		index = torrserverIndexToInternal(index)
	}

	if _, ok := r.URL.Query()["m3u"]; ok {
		h.serveM3U(w, r, hash)
		return
	}

	if _, ok := r.URL.Query()["preload"]; ok {
		h.servePreload(w, r, hash, index)
		return
	}
	if _, ok := r.URL.Query()["stat"]; ok {
		h.serveTorrentStatus(w, r, hash, index)
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
		Hash:             t.Hash,
		Title:            title,
		Name:             t.Name,
		Size:             t.Size,
		Data:             firstString(t.Data, "{}"),
		Poster:           t.Poster,
		Category:         t.Category,
		Timestamp:        time.Now().Unix(),
		StatString:       string(t.State),
		LoadedSize:       t.Downloaded,
		TorrentSize:      t.Size,
		DownloadSpeed:    t.DownloadSpeed,
		UploadSpeed:      t.UploadSpeed,
		TotalPeers:       t.PeerSummary.Known,
		PendingPeers:     t.PeerSummary.Pending,
		ActivePeers:      t.PeerSummary.Connected,
		ConnectedSeeders: t.PeerSummary.Seeds,
		HalfOpenPeers:    t.PeerSummary.HalfOpen,
		Downloaded:       t.Downloaded,
		Progress:         t.Progress,
		Stat:             t.State,
		Error:            t.Error,
	}
	if t.Title != "" {
		res.Title = t.Title
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
			ID:     internalIndexToTorrserver(f.Index),
			Path:   f.Path,
			Length: f.Size,
			Size:   f.Size,
		})
	}
	return stats
}

func (h *TorrServerHandler) serveTorrentStatus(w http.ResponseWriter, r *http.Request, hash string, index int) {
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

	target := h.preloadTarget(hash, index, size)
	res := toTorrentResponse(t, true)
	res.PreloadSize = target
	res.PreloadedBytes = h.preloadedBytes(hash, index, t, target)
	response.JSON(w, http.StatusOK, res)
}

func (h *TorrServerHandler) servePreload(w http.ResponseWriter, r *http.Request, hash string, index int) {
	t, err := h.uc.GetTorrent(hash)
	if err != nil || t == nil {
		response.Error(w, http.StatusNotFound, "Torrent not found")
		return
	}
	target := h.preloadTarget(hash, index, fileSize(t, index))
	h.setPreload(hash, index, preloadState{TargetBytes: target, StartedAt: time.Now()})

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		read, _, err := h.uc.Warmup(ctx, hash, index, target)
		if err != nil {
			logging.Debugf("preload warmup incomplete hash=%s file_index=%d read=%d: %v", hash, index, read, err)
		}
		h.setPreload(hash, index, preloadState{TargetBytes: target, ReadBytes: read, StartedAt: time.Now()})
	}()

	res := toTorrentResponse(t, true)
	res.PreloadSize = target
	res.PreloadedBytes = h.preloadedBytes(hash, index, t, target)
	response.JSON(w, http.StatusOK, res)
}

func (h *TorrServerHandler) setPreload(hash string, index int, state preloadState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.preloads[preloadKey{Hash: hash, Index: index}] = state
}

func (h *TorrServerHandler) preloadTarget(hash string, index int, size int64) int64 {
	h.mu.Lock()
	state, ok := h.preloads[preloadKey{Hash: hash, Index: index}]
	if ok && time.Since(state.StartedAt) < 10*time.Minute && state.TargetBytes > 0 {
		h.mu.Unlock()
		return state.TargetBytes
	}
	if ok {
		delete(h.preloads, preloadKey{Hash: hash, Index: index})
	}
	h.mu.Unlock()

	target := defaultPreloadSize
	if size > 0 && size < target {
		target = size
	}
	return target
}

func (h *TorrServerHandler) preloadedBytes(hash string, index int, t *models.Torrent, target int64) int64 {
	if target <= 0 {
		return 0
	}
	size := fileSize(t, index)
	if size > 0 && fileDownloaded(t, index) >= size {
		return target
	}
	if status, err := h.uc.GetCacheStatus(hash, index, 0); err == nil && status != nil {
		return minInt64(status.VirtualCacheBytes, target)
	}
	h.mu.Lock()
	state := h.preloads[preloadKey{Hash: hash, Index: index}]
	h.mu.Unlock()
	return minInt64(maxInt64(fileDownloaded(t, index), state.ReadBytes), target)
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

	h.serveStream(w, r, hash, torrserverIndexToInternal(index))
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
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("transferMode.dlna.org", "Streaming")
	if r.Header.Get("getcontentFeatures.dlna.org") != "" || r.Header.Get("GetContentFeatures.DLNA.ORG") != "" {
		w.Header().Set("contentFeatures.dlna.org", "DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000")
	}
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

type cacheRequest struct {
	Action string `json:"action"`
	Hash   string `json:"hash"`
	Link   string `json:"link"`
	Index  int    `json:"index"`
	Offset int64  `json:"offset"`
}

func decodeCacheRequest(r *http.Request) (cacheRequest, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return cacheRequest{}, err
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return cacheRequest{Action: "get"}, nil
	}
	var req cacheRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return cacheRequest{}, err
	}
	return req, nil
}

func (h *TorrServerHandler) serveM3U(w http.ResponseWriter, r *http.Request, hash string) {
	t, err := h.uc.GetTorrent(hash)
	if err != nil || t == nil {
		response.Error(w, http.StatusNotFound, "Torrent not found")
		return
	}
	w.Header().Set("Content-Type", "audio/x-mpegurl")
	fmt.Fprint(w, "#EXTM3U\n")
	host := r.Host
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	for _, f := range t.Files {
		if f != nil && f.IsMedia {
			fmt.Fprintf(w, "#EXTINF:-1,%s\n", f.Path)
			fmt.Fprintf(w, "%s://%s/stream/%s?link=%s&index=%d&play\n", scheme, host, filepath.Base(f.Path), hash, internalIndexToTorrserver(f.Index))
		}
	}
}

func normalizeTorrentLink(link string) string {
	trimmed := strings.TrimSpace(link)
	if strings.HasPrefix(trimmed, "magnet:") {
		return trimmed
	}
	if len(trimmed) == 40 || len(trimmed) == 32 {
		return "magnet:?xt=urn:btih:" + trimmed
	}
	return trimmed
}

func resolveHTTPLink(link string) (string, []byte, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest(http.MethodGet, link, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("User-Agent", "DWL/1.1.1 (Torrent)")

	resp, err := client.Do(req)
	if err != nil {
		if urlErr, ok := err.(*url.Error); ok && strings.HasPrefix(strings.ToLower(urlErr.URL), "magnet:") {
			return urlErr.URL, nil, nil
		}
		return "", nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", nil, errors.New(resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return "", nil, err
	}
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(strings.ToLower(trimmed), "magnet:") {
		return trimmed, nil, nil
	}
	return "", body, nil
}

func internalIndexToTorrserver(index int) int {
	return index + 1
}

func torrserverIndexToInternal(index int) int {
	if index <= 0 {
		return 0
	}
	return index - 1
}

func firstString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func fileDownloaded(t *models.Torrent, index int) int64 {
	if t == nil {
		return 0
	}
	for _, f := range t.Files {
		if f != nil && f.Index == index {
			return f.Downloaded
		}
	}
	return 0
}

func fileSize(t *models.Torrent, index int) int64 {
	if t == nil {
		return 0
	}
	for _, f := range t.Files {
		if f != nil && f.Index == index {
			return f.Size
		}
	}
	return 0
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func streamContentType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".avi":
		return "video/x-msvideo"
	case ".mkv":
		return "video/x-matroska"
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	case ".webm":
		return "video/webm"
	case ".ts":
		return "video/mp2t"
	}

	contentType := mime.TypeByExtension(filepath.Ext(name))
	if contentType != "" {
		return contentType
	}

	return "application/octet-stream"
}
