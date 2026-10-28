package engine

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"torrent-stream-hub/internal/config"
)

type pieceSyncPersistentCompletion struct{ storage.PieceCompletion }

func (pieceSyncPersistentCompletion) Persistent() bool { return true }

func TestEnginePieceSyncPolicy(t *testing.T) {
	for _, enableSync := range []bool{false, true} {
		t.Run(map[bool]string{false: "default skips sync", true: "explicit false restores sync"}[enableSync], func(t *testing.T) {
			dir := t.TempDir()
			cfg := &config.Config{
				DownloadDir: dir, DBPath: filepath.Join(dir, "hub.db"), TorrentPort: 0,
				BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true, BTDisableUTP: true,
				BTDisablePieceSyncConfigured: enableSync,
			}
			e, err := New(cfg, WithPieceCompletion(pieceSyncPersistentCompletion{storage.NewMapPieceCompletion()}))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(e.Close)
			data := []byte("good")
			mi := createTestTorrentBytes(4, "piece-sync", []metainfo.FileInfo{{Length: 4, Path: []string{"data"}}}, [][]byte{data})
			model, err := e.AddTorrentFile(bytes.NewReader(mi))
			if err != nil {
				t.Fatal(err)
			}
			raw := e.GetRawTorrent(model.Hash)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := raw.VerifyDataContext(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Piece(0).Storage().WriteAt(data, 0); err != nil {
				t.Fatal(err)
			}
			if err := raw.VerifyDataContext(ctx); err != nil {
				t.Fatal(err)
			}
			io, ok := raw.StorageIOTimings()
			if !ok || !raw.Piece(0).State().Complete {
				t.Fatal("hash did not publish completion", io)
			}
			if enableSync && io.CompletionSyncCount == 0 || !enableSync && io.CompletionSyncCount != 0 {
				t.Fatal("engine did not propagate piece sync policy", io)
			}
		})
	}
}
