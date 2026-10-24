package engine

import (
	"strings"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

// MetadataHandle is an opaque generation reference used to capture the final
// immutable catalog after Drop. It exposes neither readers nor readiness gates.
type MetadataHandle struct{ torrent *torrent.Torrent }

func (h MetadataHandle) Exists() bool { return h.torrent != nil }
func (h MetadataHandle) Info() (*metainfo.Info, error) {
	if h.torrent == nil || h.torrent.Info() == nil {
		return nil, nil
	}
	mi := h.torrent.Metainfo()
	info, err := mi.UnmarshalInfo()
	return &info, err
}
func (e *Engine) StorageMetadata(hash string) MetadataHandle {
	return MetadataHandle{e.GetRawTorrent(hash)}
}

func (e *Engine) PieceStatus(hash string) (string, error) {
	t := e.GetRawTorrent(hash)
	if t == nil {
		return "", ErrTorrentNotFound
	}
	info := t.Info()
	if info == nil {
		return "", nil
	}
	var out strings.Builder
	out.Grow(info.NumPieces())
	for _, run := range t.PieceStateRuns() {
		status := '0'
		if run.Complete {
			status = '2'
		} else if run.Partial || run.Hashing || run.Checking || run.Priority != 0 {
			status = '1'
		}
		for i := 0; i < run.Length; i++ {
			out.WriteRune(status)
		}
	}
	return out.String(), nil
}
