package storage

import (
	"fmt"
	"github.com/anacrolix/torrent/metainfo"
	"os"
	"path/filepath"
	"testing"
)

// Baseline and regression workload: writing outside verified ranges must not
// scan every previously verified piece. Fixed iteration counts ease comparisons.
func BenchmarkVerifiedWriteBookkeeping(b *testing.B) {
	for _, count := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "payload")
			if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
				b.Fatal(err)
			}
			st, err := os.Stat(path)
			if err != nil {
				b.Fatal(err)
			}
			fts := &fileTorrentImpl{info: &metainfo.Info{PieceLength: 16384}, client: &fileClientImpl{opts: NewFileClientOpts{VerifyOnOpen: true}}, fresh: make(map[int]pieceEvidence)}
			sig := &verifiedFileSignature{FileInfo: st, file: 0, valid: true, complete: count}
			fts.signatures = map[int]*verifiedFileSignature{0: sig}
			for i := 0; i < count; i++ {
				fts.fresh[i] = pieceEvidence{infos: []os.FileInfo{sig}}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				fts.acceptFileWrite(0, st, st, int64(count)*16384, 1)
			}
		})
	}
}
