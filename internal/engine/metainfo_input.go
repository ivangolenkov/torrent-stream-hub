package engine

import (
	"fmt"
	"io"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// Limit declared bencode string allocations as well as input bytes. A tiny
// truncated upload must not allocate the library's default 128 MiB string.
func LoadMetainfo(r io.Reader) (*metainfo.MetaInfo, error) {
	limited := &io.LimitedReader{R: r, N: 32<<20 + 1}
	decoder := bencode.NewDecoder(limited)
	decoder.MaxStrLen = 32 << 20
	var mi metainfo.MetaInfo
	if err := decoder.Decode(&mi); err != nil {
		return nil, err
	}
	if err := decoder.ReadEOF(); err != nil {
		return nil, err
	}
	if limited.N == 0 {
		return nil, fmt.Errorf("metainfo exceeds 32 MiB")
	}
	return &mi, nil
}
