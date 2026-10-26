package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// At most 1 MiB of interval entries on 64-bit systems. Overflow safely falls
// back to ordinary initial verification, not trust.
const maxInitialIncompleteRanges = 65536

// Called before publication, after import and checkpoint restore. This is not a
// live negative cache: writes cannot inherit an exemption from hash checking.
// One metadata probe per contributing file, no payload reads or piece bitmap.
func (t *fileTorrentImpl) initialIncomplete(ctx context.Context) ([]PieceRange, error) {
	var out []PieceRange
	for i, fi := range t.metainfoFileInfos {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if fi.Length == 0 {
			continue
		}
		f := t.file(i)
		var st os.FileInfo
		var err error
		if guard := t.client.opts.IOGuard; guard != nil {
			var rel string
			rel, err = filepath.Rel(t.client.opts.RootFS.Name(), f.safeOsPath)
			if err == nil {
				err = guard(rel)
			}
			if err != nil {
				t.logger().Warn("initial verification storage guard rejected evidence", "file_index", i, "err", err)
				continue
			}
		}
		if t.partFiles() {
			st, err = t.stat(f.partFilePath())
		}
		if st == nil && (err == nil || errors.Is(err, os.ErrNotExist)) {
			st, err = t.stat(f.safeOsPath)
		}
		var size int64
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				// Unknown is not absent. Keep the normal verification/error path.
				t.logger().Warn("initial verification evidence unavailable", "file_index", i, "err", err)
				continue
			}
		} else {
			size = st.Size()
		}
		if size >= fi.Length {
			continue
		}
		begin := int((fi.TorrentOffset + size) / t.info.PieceLength)
		end := int((fi.TorrentOffset+fi.Length-1)/t.info.PieceLength) + 1
		if len(out) != 0 && out[len(out)-1].End >= begin {
			out[len(out)-1].End = max(out[len(out)-1].End, end)
		} else {
			if out == nil {
				out = make([]PieceRange, 0, min(len(t.files), maxInitialIncompleteRanges))
			}
			if len(out) == maxInitialIncompleteRanges {
				t.logger().Warn("initial verification plan exceeds interval budget")
				return nil, ctx.Err()
			}
			out = append(out, PieceRange{begin, end})
		}
	}
	return out, ctx.Err()
}
