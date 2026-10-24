package budget

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"torrent-stream-hub/internal/delivery/http/response"
	"torrent-stream-hub/internal/metafetch"
)

type Limits struct {
	Control, Torrent, Envelope int64
	ReadTimeout                time.Duration
}

func Defaults() Limits {
	return Limits{Control: 1 << 20, Torrent: 32 << 20, Envelope: 34 << 20, ReadTimeout: 30 * time.Second}
}

var ErrBusy = errors.New("metainfo job limit reached")

// A rejected/unfinished HTTP/1 body must not be drained after its read deadline
// is cleared. Otherwise net/http can wait for the slow sender before flushing
// the error response. HTTP/2 closes only the request stream.
func BodyError(w http.ResponseWriter, r *http.Request, status int, message string) {
	if r.ProtoMajor == 1 {
		w.Header().Set("Connection", "close")
		r.Close = true
	}
	// finishRequest closes its original body even with Connection: close.
	// Close it under an expired read deadline now, before middleware resets the
	// deadline; otherwise its bounded automatic drain can wait on a slow sender.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now())
	if r.Body != nil {
		_ = r.Body.Close()
	}
	response.Error(w, status, message)
}

func Status(err error, fallback ...int) int {
	var oversized *http.MaxBytesError
	var timeout net.Error
	var disk *os.PathError
	switch {
	case errors.As(err, &oversized), errors.Is(err, metafetch.ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, ErrBusy):
		return http.StatusTooManyRequests
	case errors.Is(err, metafetch.ErrPolicy):
		return http.StatusForbidden
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return http.StatusRequestTimeout
	case errors.Is(err, context.Canceled):
		return http.StatusServiceUnavailable
	case errors.As(err, &disk):
		return http.StatusInsufficientStorage
	default:
		if len(fallback) > 0 {
			return fallback[0]
		}
		return http.StatusBadRequest
	}
}

// Control bodies are fully bounded before parsing, including endpoints which
// historically ignored decoder errors. Uploads stream under their envelope cap.
func Bodies(limits Limits) func(http.Handler) http.Handler {
	if limits.Control <= 0 || limits.Torrent <= 0 || limits.Envelope < limits.Torrent || limits.ReadTimeout <= 0 {
		panic("invalid HTTP body limits")
	}
	readers := make(chan struct{}, 32)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body == nil || r.Body == http.NoBody {
				next.ServeHTTP(w, r)
				return
			}
			controller := http.NewResponseController(w)
			_ = controller.SetReadDeadline(time.Now().Add(limits.ReadTimeout))
			interrupted := make(chan struct{})
			stopInterrupt := context.AfterFunc(r.Context(), func() {
				_ = controller.SetReadDeadline(time.Now())
				close(interrupted)
			})
			defer func() {
				if !stopInterrupt() {
					<-interrupted
				}
				_ = controller.SetReadDeadline(time.Time{})
			}()
			limit := limits.Control
			upload := r.Method == http.MethodPost && r.URL.Path == "/torrent/upload"
			if upload {
				limit = limits.Envelope
			}
			if r.ContentLength > limit {
				BodyError(w, r, http.StatusRequestEntityTooLarge, "Request body exceeds limit")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			if upload {
				next.ServeHTTP(w, r)
				return
			}
			select {
			case readers <- struct{}{}:
			default:
				BodyError(w, r, http.StatusTooManyRequests, "Control body reader limit reached")
				return
			}
			// Retain the slot until the parsed command returns: otherwise blocked
			// handlers could retain unlimited 1 MiB buffers after the read ends.
			defer func() { <-readers }()
			body, err := io.ReadAll(r.Body)
			if err != nil {
				BodyError(w, r, Status(err), "Cannot read request body within limits")
				return
			}
			_ = controller.SetReadDeadline(time.Time{})
			r.Body = io.NopCloser(bytes.NewReader(body))
			next.ServeHTTP(w, r)
		})
	}
}

type Jobs struct {
	slots  chan struct{}
	Dir    string
	Limits Limits
}

func NewJobs(dir string) *Jobs {
	return &Jobs{slots: make(chan struct{}, 2), Dir: dir, Limits: Defaults()}
}
func (b *Jobs) Acquire(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case b.slots <- struct{}{}:
	default:
		return nil, ErrBusy
	}
	var once sync.Once
	return func() { once.Do(func() { <-b.slots }) }, nil
}
