package api

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"torrent-stream-hub/internal/delivery/http/response"
	"torrent-stream-hub/internal/logging"
)

func parseLogSelection(query url.Values, now time.Time) (logging.Selection, error) {
	invalid := errors.New("invalid log parameters: use date or from/to in YYYY-MM-DD format, tail between 1 and 100000")
	for _, key := range []string{"date", "from", "to", "tail"} {
		if values, ok := query[key]; ok && (len(values) != 1 || values[0] == "") {
			return logging.Selection{}, invalid
		}
	}
	date, from, to := query.Get("date"), query.Get("from"), query.Get("to")
	if date != "" && (from != "" || to != "") {
		return logging.Selection{}, invalid
	}
	if (from == "") != (to == "") {
		return logging.Selection{}, invalid
	}
	if from == "" {
		if date == "" {
			date = now.Format("2006-01-02")
		}
		from, to = date, date
	}
	for _, value := range []string{from, to} {
		parsed, err := time.Parse("2006-01-02", value)
		if err != nil || parsed.Format("2006-01-02") != value {
			return logging.Selection{}, invalid
		}
	}
	if from > to {
		return logging.Selection{}, invalid
	}
	selection := logging.Selection{From: from, To: to}
	if raw := query.Get("tail"); raw != "" {
		for _, c := range raw {
			if c < '0' || c > '9' {
				return logging.Selection{}, invalid
			}
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > 100000 {
			return logging.Selection{}, invalid
		}
		selection.Tail = n
	}
	return selection, nil
}

func LogsHandler(reader logging.LogReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			response.Error(w, http.StatusBadRequest, "invalid query")
			return
		}
		selection, err := parseLogSelection(query, time.Now())
		if err != nil {
			response.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		if reader == nil {
			response.Error(w, http.StatusNotFound, "logs not found")
			return
		}
		stream, err := reader.OpenLogs(r.Context(), selection)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				response.Error(w, http.StatusNotFound, "logs not found")
			} else {
				logging.Warnf("open logs failed: %v", err)
				response.Error(w, http.StatusInternalServerError, "unable to read logs")
			}
			return
		}
		defer stream.Close()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if _, err := io.Copy(w, stream); err != nil {
			logging.Warnf("log transfer failed: %v", err)
		}
	}
}
