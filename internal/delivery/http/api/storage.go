package api

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/go-chi/chi/v5"
	"net/http"
	"torrent-stream-hub/internal/delivery/http/response"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
	"torrent-stream-hub/internal/usecase"
)

func (h *APIHandler) StorageCommand(w http.ResponseWriter, r *http.Request) {
	var cmd models.StorageCommand
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&cmd); err != nil {
		response.Error(w, http.StatusBadRequest, "Invalid storage command")
		return
	}
	switch cmd.Action {
	case "plan_move", "apply_move", "retry_storage", "cancel_storage", "analyze_location", "apply_location", "reconcile_storage":
	default:
		response.Error(w, http.StatusBadRequest, "Unsupported storage action")
		return
	}
	if cmd.RecordID == "" || cmd.StorageID == "" || cmd.MappingRevision < 1 || cmd.Version < 0 {
		response.Error(w, http.StatusBadRequest, "Storage identity and revision are required")
		return
	}
	location, err := h.uc.StorageCommand(r.Context(), chi.URLParam(r, "hash"), cmd)
	if err != nil {
		status, code := http.StatusInternalServerError, "storage_error"
		switch {
		case errors.Is(err, usecase.ErrTorrentNotFound):
			status, code = http.StatusNotFound, "not_found"
		case errors.Is(err, repository.ErrStorageQueueFull):
			status, code = http.StatusTooManyRequests, "queue_full"
			w.Header().Set("Retry-After", "1")
		case errors.Is(err, usecase.ErrStorageConflict) || errors.Is(err, repository.ErrStateConflict) || errors.Is(err, usecase.ErrMetadataPending):
			status, code = http.StatusConflict, "state_conflict"
		case errors.Is(err, context.Canceled):
			status, code = http.StatusServiceUnavailable, "shutdown"
		}
		response.JSON(w, status, map[string]any{"error": err.Error(), "code": code, "retryable": status == http.StatusTooManyRequests})
		return
	}
	if h.syncWorker != nil {
		h.syncWorker.Refresh()
	}
	response.JSON(w, http.StatusAccepted, location)
}
