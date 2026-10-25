package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
	"torrent-stream-hub/internal/engine"

	"torrent-stream-hub/internal/delivery/http/response"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/usecase"

	"github.com/go-chi/chi/v5"
)

type APIHandler struct {
	uc              *usecase.TorrentUseCase
	syncWorker      *usecase.SyncWorker
	sseWriteTimeout time.Duration
}

func NewAPIHandler(uc *usecase.TorrentUseCase, sw *usecase.SyncWorker) *APIHandler {
	return &APIHandler{
		uc:              uc,
		syncWorker:      sw,
		sseWriteTimeout: 10 * time.Second,
	}
}

func (h *APIHandler) RegisterRoutes(r chi.Router) {
	r.Get("/torrents", h.GetAllTorrents)
	r.Get("/torrent/{hash}", h.GetTorrent)
	r.Post("/torrent/add", h.AddTorrent)
	r.Post("/torrent/{hash}/action", h.TorrentAction)
	r.Post("/torrent/{hash}/storage", h.StorageCommand)
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
	version := h.uc.NextSnapshotVersion()
	torrents, err := h.uc.GetAllTorrents()
	if err != nil {
		logging.Warnf("api get torrents failed: %v", err)
		if errors.Is(err, usecase.ErrSnapshotConflict) {
			response.Error(w, http.StatusConflict, err.Error())
			return
		}
		response.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	logging.Debugf("api get torrents count=%d", len(torrents))
	w.Header().Set("X-Hub-Snapshot", version)
	if r.URL.Query().Get("view") == "compact" {
		torrents = usecase.CompactTorrents(torrents)
	}
	response.JSON(w, http.StatusOK, torrents)
}

type AddTorrentReq struct {
	FolderName  string `json:"folder_name"`
	PrivateSafe bool   `json:"private_safe"`
	Link        string `json:"link"`
	SavePath    string `json:"save_path"`
	Sequential  bool   `json:"sequential"`
	Poster      string `json:"poster"`
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
	t, err := h.uc.AddMagnetWithMetadata(req.Link, usecase.TorrentMetadata{Poster: req.Poster, PrivateSafe: req.PrivateSafe, FolderName: req.FolderName})
	if err != nil {
		if errors.Is(err, engine.ErrDownloadBlocked) || errors.Is(err, usecase.ErrStorageConflict) {
			response.Error(w, http.StatusConflict, err.Error())
			return
		}
		logging.Warnf("api add torrent failed %s: %v", logging.SafeMagnetSummary(req.Link), err)
		response.Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	response.JSON(w, http.StatusAccepted, t)
}

type ActionReq struct {
	Action      string `json:"action"`
	DeleteFiles bool   `json:"delete_files"`
	RecordID    string `json:"record_id"`
	Version     int64  `json:"version,string"`
	Choice      string `json:"choice"`
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
	accepted := false
	switch req.Action {
	case "retry_deletion":
		err = h.uc.RetryDeletion(r.Context(), hash, req.RecordID, req.Version)
	case "cancel_deletion":
		err = h.uc.CancelDeletion(r.Context(), hash, req.RecordID, req.Version)
	case "forget_record":
		err = h.uc.ForgetRecovered(r.Context(), hash, req.RecordID, req.Version)
	case "retry_ownership":
		err = h.uc.RetryOwnership(hash, req.RecordID, req.Version)
		accepted = true
	case "analyze_legacy", "apply_legacy", "resume_verify":
		err = h.uc.StartRecovery(r.Context(), hash, req.RecordID, req.Version, req.Action, req.Choice)
		accepted = true
	case "make_permanent":
		err = h.uc.MakePermanent(hash, r.Context())
	case "pause":
		err = h.uc.Pause(hash, r.Context())
	case "resume":
		err = h.uc.Resume(hash, r.Context())
	case "delete":
		err = h.uc.Delete(hash, req.DeleteFiles, r.Context())
	case "recheck":
		operation, opErr := h.uc.Recheck(hash, r.Context())
		if opErr == nil {
			w.Header().Set("X-Hub-Snapshot", h.uc.NextSnapshotVersion())
			response.JSON(w, http.StatusAccepted, operation)
			return
		}
		err = opErr
	default:
		logging.Debugf("api torrent action unknown hash=%s action=%s", hash, req.Action)
		response.Error(w, http.StatusBadRequest, "Unknown action")
		return
	}

	if err != nil {
		var storageErr *usecase.StorageOperationError
		if errors.As(err, &storageErr) || errors.Is(err, usecase.ErrOwnershipCatalogChanged) || errors.Is(err, usecase.ErrStorageConflict) || errors.Is(err, usecase.ErrDeletionCancellation) {
			code := models.StorageCatalogChanged
			if storageErr != nil {
				code = storageErr.Code
			}
			t, readErr := h.uc.GetTorrent(hash)
			if readErr == nil && t != nil && t.StorageRecovery != nil {
				if storageErr == nil {
					code = t.StorageRecovery.Reason
				}
				response.JSON(w, http.StatusConflict, models.StorageActionError{Error: "Storage operation requires recovery or a fresh operation version", Code: code, StorageRecovery: t.StorageRecovery})
				return
			}
			if errors.Is(err, usecase.ErrOwnershipCatalogChanged) {
				response.JSON(w, http.StatusConflict, models.StorageActionError{Error: "Storage catalog changed; retry shortly", Code: code})
				return
			}
			if storageErr != nil {
				response.JSON(w, http.StatusConflict, models.StorageActionError{Error: "Storage operation requires recovery", Code: storageErr.Code})
				return
			}
			if errors.Is(err, usecase.ErrDeletionCancellation) {
				response.JSON(w, http.StatusConflict, models.StorageActionError{Error: "Deletion cancellation requested", Code: models.StorageDeletionCancelled})
				return
			}
		}
		if errors.Is(err, engine.ErrOperationConflict) || errors.Is(err, usecase.ErrMetadataPending) {
			response.Error(w, http.StatusConflict, err.Error())
			return
		}
		if errors.Is(err, engine.ErrDownloadBlocked) {
			response.Error(w, http.StatusConflict, err.Error())
			return
		}
		if errors.Is(err, usecase.ErrStorageConflict) {
			response.Error(w, http.StatusConflict, "Torrent is in use or storage ownership is unconfirmed")
			return
		}
		if errors.Is(err, usecase.ErrTorrentNotFound) {
			logging.Debugf("api torrent action not found hash=%s action=%s", hash, req.Action)
			response.Error(w, http.StatusNotFound, err.Error())
			return
		}
		logging.Warnf("api torrent action failed hash=%s action=%s: %v", hash, req.Action, err)
		response.Error(w, http.StatusInternalServerError, "Torrent action failed")
		return
	}

	if accepted {
		w.Header().Set("X-Hub-Snapshot", h.uc.NextSnapshotVersion())
		response.JSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "command_at_ns": time.Now().UnixNano()})
		return
	}
	h.commandStatus(w)
}

func (h *APIHandler) GetTorrent(w http.ResponseWriter, r *http.Request) {
	t, err := h.uc.GetTorrent(chi.URLParam(r, "hash"))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "Cannot read torrent")
		return
	}
	if t == nil {
		response.Error(w, http.StatusNotFound, "Torrent not found")
		return
	}
	response.JSON(w, http.StatusOK, t)
}

type PriorityReq struct {
	Priority *int `json:"priority"`
}

func (h *APIHandler) SetTorrentPriority(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	var req PriorityReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if req.Priority == nil {
		response.Error(w, http.StatusBadRequest, "Priority is required")
		return
	}
	err := h.uc.SetTorrentFilesPriority(hash, models.FilePriority(*req.Priority), r.Context())
	if err != nil {
		if priorityError(w, err) {
			return
		}
		if errors.Is(err, usecase.ErrTorrentNotFound) {
			response.Error(w, http.StatusNotFound, "Torrent not found")
			return
		}
		response.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.commandStatus(w)
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

	if req.Priority == nil {
		response.Error(w, http.StatusBadRequest, "Priority is required")
		return
	}
	err = h.uc.SetFilePriority(hash, index, models.FilePriority(*req.Priority), r.Context())
	if err != nil {
		if priorityError(w, err) {
			return
		}
		if errors.Is(err, usecase.ErrTorrentNotFound) {
			response.Error(w, http.StatusNotFound, "Torrent not found")
			return
		}
		response.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.commandStatus(w)
}

func (h *APIHandler) commandStatus(w http.ResponseWriter) {
	w.Header().Set("X-Hub-Snapshot", h.uc.NextSnapshotVersion())
	response.JSON(w, http.StatusOK, map[string]any{"status": "ok", "command_at_ns": time.Now().UnixNano()})
}

func priorityError(w http.ResponseWriter, err error) bool {
	status := 0
	switch {
	case errors.Is(err, usecase.ErrInvalidPriority):
		status = http.StatusBadRequest
	case errors.Is(err, usecase.ErrFileNotFound), errors.Is(err, usecase.ErrTorrentNotFound):
		status = http.StatusNotFound
	case errors.Is(err, usecase.ErrMetadataPending), errors.Is(err, usecase.ErrStorageConflict), errors.Is(err, engine.ErrDownloadBlocked):
		status = http.StatusConflict
	}
	if status == 0 {
		return false
	}
	response.Error(w, status, err.Error())
	return true
}

func (h *APIHandler) GetTorrentFiles(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	t, err := h.uc.GetTorrent(hash)
	if err != nil {
		logging.Warnf("api get torrent files failed hash=%s: %v", hash, err)
		if errors.Is(err, usecase.ErrSnapshotConflict) {
			response.Error(w, http.StatusConflict, err.Error())
			return
		}
		response.Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	if t == nil {
		logging.Debugf("api get torrent files not found hash=%s", hash)
		response.Error(w, http.StatusNotFound, "Torrent not found")
		return
	}

	if t.Control != nil {
		revision := ""
		if t.Storage != nil {
			revision = strconv.FormatInt(t.Storage.Revision, 10)
		}
		w.Header().Set("X-Hub-Details", fmt.Sprintf("%s/%s/%d/%s/%s", t.Control.RecordID, t.CatalogVersion, t.Control.DesiredRevision, revision, t.Control.RuntimeAttachment))
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
	controller := http.NewResponseController(w)
	// A bounded writer is required; reject unsupported middleware/writers rather
	// than pretending that shutdown can interrupt an arbitrary blocking Write.
	if err := controller.SetWriteDeadline(time.Now().Add(h.sseWriteTimeout)); err != nil {
		response.Error(w, http.StatusServiceUnavailable, "SSE write deadlines are unavailable")
		return
	}
	defer controller.SetWriteDeadline(time.Time{})
	if _, ok := w.(http.Flusher); !ok {
		response.Error(w, http.StatusServiceUnavailable, "SSE flushing is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan usecase.PublishedSnapshot, 1)
	if !h.syncWorker.AddSnapshotClient(ch) {
		response.Error(w, http.StatusServiceUnavailable, "SSE subscriber limit reached")
		return
	}
	defer h.syncWorker.RemoveSnapshotClient(ch)
	logging.Infof("api sse client connected remote=%s", r.RemoteAddr)
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	write := func(message string) bool {
		if err := controller.SetWriteDeadline(time.Now().Add(h.sseWriteTimeout)); err != nil {
			return false
		}
		if _, err := io.WriteString(w, message); err != nil {
			return false
		}
		return controller.Flush() == nil
	}
	// Establish the subscription even if startup has not published a snapshot.
	// Comments keep EventSource connected without implying an empty torrent list.
	if !write(": connected\n\n") {
		return
	}

	for {
		select {
		case data, ok := <-ch:
			if !ok {
				return
			}
			payload := data.Data
			if r.URL.Query().Get("view") == "compact" && data.Compact != nil {
				payload = data.Compact
			}
			if !write(fmt.Sprintf("id: %s\ndata: %s\n\n", data.Version, payload)) {
				return
			}
		case <-heartbeat.C:
			if !write(": heartbeat\n\n") {
				return
			}
		case <-h.syncWorker.Done():
			return
		case <-r.Context().Done():
			logging.Infof("api sse client disconnected remote=%s", r.RemoteAddr)
			return
		}
	}
}
