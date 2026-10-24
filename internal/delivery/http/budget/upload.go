package budget

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"torrent-stream-hub/internal/metafetch"
)

// ReadUpload has no ParseMultipartForm spill outside the managed directory.
// One job retains one file <=32 MiB and fields <=64 KiB, under a 34 MiB envelope.
func (b *Jobs) ReadUpload(w http.ResponseWriter, r *http.Request) (file *os.File, fields map[string]string, cleanup func(), err error) {
	cleanup = func() {}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(b.Limits.ReadTimeout))
	defer controller.SetReadDeadline(time.Time{})
	r.Body = http.MaxBytesReader(w, r.Body, b.Limits.Envelope)
	reader, err := r.MultipartReader()
	if err != nil {
		return
	}
	fields = make(map[string]string)
	var name string
	cleanup = func() {
		if file != nil {
			file.Close()
		}
		if name != "" {
			_ = os.Remove(name)
		}
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	var fieldsBytes int64
	for count := 0; ; count++ {
		if err = r.Context().Err(); err != nil {
			return
		}
		var part interface {
			io.Reader
			Close() error
			FormName() string
			FileName() string
		}
		part, err = reader.NextPart()
		if err == io.EOF {
			err = nil
			break
		}
		if err != nil {
			return
		}
		if count >= 16 {
			err = metafetch.ErrTooLarge
			return
		}
		if part.FileName() != "" {
			if file != nil || part.FormName() != "file" {
				err = fmt.Errorf("expected one torrent file")
				return
			}
			if err = os.MkdirAll(b.Dir, 0700); err != nil {
				return
			}
			file, err = os.CreateTemp(b.Dir, "metainfo-*.tmp")
			if err != nil {
				return
			}
			name = file.Name()
			var written int64
			written, err = io.Copy(file, io.LimitReader(part, b.Limits.Torrent+1))
			if err != nil {
				return
			}
			if written > b.Limits.Torrent {
				err = metafetch.ErrTooLarge
				return
			}
		} else {
			var value []byte
			value, err = io.ReadAll(io.LimitReader(part, 64<<10+1))
			if err != nil {
				return
			}
			fieldsBytes += int64(len(value))
			if fieldsBytes > 64<<10 {
				err = metafetch.ErrTooLarge
				return
			}
			fields[part.FormName()] = string(value)
		}
		if err = part.Close(); err != nil {
			return
		}
	}
	if file == nil {
		err = fmt.Errorf("torrent file is required")
		return
	}
	// Consume the bounded multipart epilogue while the body deadline still
	// applies. Successful parsing alone does not imply the HTTP body ended.
	if _, err = io.Copy(io.Discard, r.Body); err != nil {
		return
	}
	_, err = file.Seek(0, io.SeekStart)
	return
}
