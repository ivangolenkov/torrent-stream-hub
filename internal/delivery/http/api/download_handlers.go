package api

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"torrent-stream-hub/internal/delivery/http/response"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/usecase"

	"github.com/go-chi/chi/v5"
)

func (h *APIHandler) DownloadTorrentFile(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	release, err := h.uc.AcquireTransfer(hash)
	if err != nil {
		h.handleDownloadError(w, hash, err)
		return
	}
	defer release()
	index, err := strconv.Atoi(chi.URLParam(r, "index"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "Invalid file index")
		return
	}

	_, file, err := h.uc.GetDownloadedFile(hash, index)
	if err != nil {
		h.handleDownloadError(w, hash, err)
		return
	}
	h.serveDiskFile(w, r, file)
}

func (h *APIHandler) DownloadTorrent(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	release, err := h.uc.AcquireTransfer(hash)
	if err != nil {
		h.handleDownloadError(w, hash, err)
		return
	}
	defer release()
	t, files, err := h.uc.GetDownloadedTorrentFiles(hash)
	if err != nil {
		h.handleDownloadError(w, hash, err)
		return
	}

	if len(files) == 1 {
		h.serveDiskFile(w, r, files[0])
		return
	}

	if r.Header.Get("Range") != "" {
		w.Header().Set("Content-Range", "bytes */*")
		response.Error(w, http.StatusRequestedRangeNotSatisfiable, "Range is not supported for zip downloads")
		return
	}

	archiveName := safeAttachmentName(t.Name)
	if archiveName == "" {
		archiveName = hash
	}
	archiveName += ".zip"
	// Validate ALL entry names before committing response headers.
	for _, file := range files {
		if _, err := safeZipEntryName(file.Path); err != nil {
			h.handleDownloadError(w, hash, usecase.ErrStorageConflict)
			return
		}
	}

	w.Header().Set("Content-Type", "application/zip")
	setAttachment(w, archiveName)

	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	zipWriter := zip.NewWriter(w)

	for _, file := range files {
		if err := r.Context().Err(); err != nil {
			return
		}

		entryName, err := safeZipEntryName(file.Path)
		if err != nil {
			logging.Warnf("zip entry rejected hash=%s path=%q: %v", hash, file.Path, err)
			return
		}

		in, err := file.Open()
		if err != nil {
			logging.Warnf("zip file open failed hash=%s entry=%q: %v", hash, file.Path, err)
			return
		}

		// Store files without compression: downloads are local-network oriented and
		// compression makes on-the-fly ZIP generation CPU-bound on weak devices.
		header := &zip.FileHeader{Name: entryName, Method: zip.Store}
		header.SetModTime(time.Now())
		header.SetMode(0o644)
		entry, err := zipWriter.CreateHeader(header)
		if err != nil {
			in.Close()
			logging.Warnf("zip entry create failed hash=%s entry=%q: %v", hash, entryName, err)
			return
		}
		_, copyErr := io.CopyBuffer(entry, contextReader{ctx: r.Context(), reader: in}, make([]byte, 1024*1024))
		closeErr := in.Close()
		if copyErr != nil {
			logging.Warnf("zip file copy failed hash=%s entry=%q: %v", hash, file.Path, copyErr)
			return
		}
		if closeErr != nil {
			logging.Warnf("zip file close failed hash=%s entry=%q: %v", hash, file.Path, closeErr)
			return
		}
	}
	// Only complete the central directory after every entry succeeded. A failed
	// transfer must not masquerade as a valid ZIP with silently missing files.
	_ = zipWriter.Close()
}

func (h *APIHandler) serveDiskFile(w http.ResponseWriter, r *http.Request, file usecase.DownloadedDiskFile) {
	in, err := file.Open()
	if err != nil {
		h.handleDownloadError(w, "", err)
		return
	}
	defer in.Close()

	stat, err := in.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() != file.Size {
		response.Error(w, http.StatusConflict, "Downloaded file is not available on disk")
		return
	}

	filename := safeAttachmentName(filepath.Base(file.Path))
	if filename == "" {
		filename = fmt.Sprintf("file-%d", file.Index)
	}

	w.Header().Set("Accept-Ranges", "bytes")
	setAttachment(w, filename)
	if contentType := mime.TypeByExtension(filepath.Ext(filename)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	w.Header().Set("Last-Modified", stat.ModTime().UTC().Format(http.TimeFormat))

	if r.Header.Get("Range") != "" {
		http.ServeContent(w, r, filename, stat.ModTime(), in)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	_, _ = io.CopyBuffer(w, contextReader{ctx: r.Context(), reader: in}, make([]byte, 1024*1024))
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (h *APIHandler) handleDownloadError(w http.ResponseWriter, hash string, err error) {
	if errors.Is(err, usecase.ErrTorrentNotFound) || errors.Is(err, os.ErrNotExist) {
		response.Error(w, http.StatusNotFound, "Torrent not found")
		return
	}
	if errors.Is(err, usecase.ErrDownloadNotReady) || errors.Is(err, usecase.ErrStorageConflict) || errors.Is(err, usecase.ErrMetadataPending) || errors.Is(err, engine.ErrOperationConflict) {
		response.Error(w, http.StatusConflict, "Download is not ready")
		return
	}
	if strings.Contains(err.Error(), "file index out of bounds") {
		response.Error(w, http.StatusBadRequest, "File index out of bounds")
		return
	}
	if strings.Contains(err.Error(), "outside download dir") {
		response.Error(w, http.StatusBadRequest, "Unsafe download path")
		return
	}
	logging.Warnf("download failed hash=%s: %v", hash, err)
	response.Error(w, http.StatusInternalServerError, "Download storage operation failed")
}

func setAttachment(w http.ResponseWriter, filename string) {
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s", asciiAttachmentName(filename), url.PathEscape(filename)))
}

func asciiAttachmentName(filename string) string {
	if filename == "" {
		return "download"
	}
	var b strings.Builder
	for _, r := range filename {
		if r >= 32 && r < 127 && r != '"' && r != '\\' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func safeAttachmentName(filename string) string {
	filename = strings.TrimSpace(filepath.Base(filename))
	filename = strings.Trim(filename, ".")
	return filename
}

func safeZipEntryName(name string) (string, error) {
	entry := filepath.ToSlash(strings.TrimSpace(name))
	entry = path.Clean(entry)
	if entry == "." || entry == ".." || strings.Contains(entry, "\\") || strings.Contains(entry, ":") || strings.HasPrefix(entry, "../") || strings.HasPrefix(entry, "/") {
		return "", fmt.Errorf("unsafe zip entry path: %s", name)
	}
	return entry, nil
}
