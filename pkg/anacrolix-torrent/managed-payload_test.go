package torrent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

func TestManagedPayloadExplicitRecheck(t *testing.T) {
	for _, mode := range []string{"v1", "v2", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			dir, mi, data, _ := warmCheckpoint(t, mode)
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			backend := storage.NewFileOpts(storage.NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, ManagedPayload: true, UsePartFiles: g.Some(false), NoCompletionRename: true, PieceCompletion: storage.NewMapPieceCompletion(),
				TorrentPathResolver: func(base string, info *metainfo.Info, _ metainfo.Hash) (string, []string, error) {
					return filepath.Join(base, "payload", info.Name), []string{"video.bin"}, nil
				}})
			t.Cleanup(func() { backend.Close() })
			cfg := TestingConfig(t)
			cfg.DisableTCP, cfg.DisableUTP, cfg.DisableWebtorrent = true, true, true
			cfg.DefaultStorage = backend
			cl, err := NewClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cl.Close() })
			tor := addCheckpointTorrent(t, cl, mi, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := tor.VerifyDataContext(ctx); err != nil {
				t.Fatal(err)
			}
			if tor.BytesCompleted() != int64(len(data)) {
				t.Fatal("initial hash did not complete payload")
			}
			// Exercise actual concurrent backend writes, including independent
			// chunks of one piece, then validate with the library's real v1/v2
			// hash path (not just storage's publication callback).
			cl.rLock()
			attachment := tor.storage
			cl.rUnlock()
			info := tor.Info()
			const chunk = 1024
			errors := make(chan error, (len(data)+chunk-1)/chunk)
			for off := 0; off < len(data); off += chunk {
				go func() {
					piece := attachment.TorrentImpl.Piece(info.Piece(off / int(info.PieceLength)))
					_, err := piece.WriteAt(data[off:min(off+chunk, len(data))], int64(off)%info.PieceLength)
					errors <- err
				}()
			}
			for off := 0; off < len(data); off += chunk {
				if err := <-errors; err != nil {
					t.Error(err)
				}
			}
			for i := 0; i < info.NumPieces(); i++ {
				if attachment.CachedComplete(i) {
					t.Fatal("concurrent writes retained old completion")
				}
			}
			if err := tor.VerifyDataContext(ctx); err != nil {
				t.Fatal(err)
			}
			if tor.BytesCompleted() != int64(len(data)) {
				t.Fatal("parallel writes failed actual hash verification")
			}
			before, ok := tor.StorageIOTimings()
			if !ok || !before.ManagedPayload || before.FileCacheOpen != 1 {
				t.Fatal(before, ok)
			}
			path := filepath.Join(dir, "payload", "film", "video.bin")
			// The old cached descriptor still refers to valid original bytes.
			// Recheck must close it and hash the replacement inode, not merely
			// read modified bytes through an already-open descriptor.
			if err := os.Rename(path, path+".old"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, make([]byte, len(data)), 0600); err != nil {
				t.Fatal(err)
			}
			if err := tor.VerifyDataContext(ctx); err != nil {
				t.Fatal(err)
			}
			if tor.BytesCompleted() != 0 {
				t.Fatal("Recheck retained external corruption", tor.BytesCompleted())
			}
			after, _ := tor.StorageIOTimings()
			if after.FileCacheMisses <= before.FileCacheMisses {
				t.Fatal("explicit recheck did not reacquire descriptor", before, after)
			}
		})
	}
}
