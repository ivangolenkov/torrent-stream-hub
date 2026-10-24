package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// VerifyStorage performs an offline hash pass. Invalid/missing pieces remain
// incomplete, not trusted; neither payload download nor discovery is enabled.
// This read-only migration pass understands the old .part layout without
// renaming journal-owned copies. The runtime imports these names after cleanup.
func VerifyStorage(ctx context.Context, dir, hash string, mi *metainfo.MetaInfo) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	backend := storage.NewFileOpts(storage.NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, NoCompletionRename: true, IOContext: ctx,
		TorrentDirMaker: func(base string, _ *metainfo.Info, _ metainfo.Hash) string {
			return filepath.Join(base, payload.Root(hash))
		},
	})
	defer backend.Close()
	cfg := torrent.NewDefaultClientConfig()
	cfg.DefaultStorage = backend
	cfg.NoDHT = true
	cfg.DisablePEX = true
	cfg.DisableTCP = true
	cfg.DisableUTP = true
	cfg.DisableTrackers = true
	cfg.DisableWebtorrent = true
	cfg.DisableWebseeds = true
	cfg.NoDefaultPortForwarding = true
	cfg.NoUpload = true
	cfg.AcceptPeerConnections = false
	cfg.PieceHashersPerTorrent = 1
	cfg.MaxActivePieceHashers = 1
	client, err := torrent.NewClient(cfg)
	if err != nil {
		return err
	}
	defer client.Close()
	spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		return err
	}
	spec.DisallowDataDownload = true
	spec.DisallowDataUpload = true
	spec.Trackers = nil
	spec.Webseeds = nil
	spec.Sources = nil
	spec.PeerAddrs = nil
	spec.DhtNodes = nil
	t, _, err := client.AddTorrentSpec(spec)
	if err != nil {
		return err
	}
	return t.VerifyDataContext(ctx)
}

func (e *Engine) RunStorageTask(fn func(context.Context)) bool {
	return e.StartTask(fn)
}

func (e *Engine) CheckMigrationSpace(path string, bytes uint64) error {
	free, err := GetFreeSpace(path)
	if err != nil {
		return err
	}
	reserve := e.minimumDiskBytes()
	if free < reserve || bytes > free-reserve {
		return fmt.Errorf("insufficient free space for safe migration copy")
	}
	return nil
}

// AddPausedMetainfo permits background verification without payload admission.
func (e *Engine) AddPausedMetainfo(mi *metainfo.MetaInfo, identity ...string) (*torrent.Torrent, error) {
	if !e.beginWork() {
		return nil, context.Canceled
	}
	defer e.wg.Done()
	spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		return nil, err
	}
	spec.DisallowDataDownload = true
	t, _, err := e.client.AddTorrentSpec(spec)
	if err != nil {
		return nil, err
	}
	model, err := e.registerTorrent(t, "", &models.Torrent{State: models.StatePaused}, identity...)
	if err != nil {
		t.Drop()
		return nil, err
	}
	if err := e.FinishRestore(model.Hash, models.StatePaused); err != nil {
		return nil, err
	}
	return t, nil
}
