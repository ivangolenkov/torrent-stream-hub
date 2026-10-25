package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// VerifyStorage performs an offline hash pass. Invalid/missing pieces remain
// incomplete, not trusted; neither payload download nor discovery is enabled.
// This read-only migration pass understands the old .part layout without
// renaming journal-owned copies. The runtime imports these names after cleanup.
func VerifyStorage(ctx context.Context, dir, hash string, mi *metainfo.MetaInfo) error {
	_, err := verifyStorage(ctx, dir, payload.Root(hash), mi, nil, nil, false)
	return err
}

type FileVerification struct {
	ValidPieces int `json:"valid_pieces"`
	TotalPieces int `json:"total_pieces"`
}
type StorageVerification struct {
	Files                    []FileVerification
	ValidPieces, TotalPieces int
}

// VerifyReadOnlyStorage uses the library's v1/v2/hybrid verifier, including v1
// shared boundary pieces and v2 alignment. Overrides select whole files only;
// no synthesis/merging of pieces and no persistent completion authority.
func VerifyReadOnlyStorage(ctx context.Context, dir, rootPath string, mi *metainfo.MetaInfo, overrides map[string]string, onRead func(string, int64)) (*StorageVerification, error) {
	return verifyStorage(ctx, dir, rootPath, mi, overrides, onRead, true)
}
func VerifyStorageProgress(ctx context.Context, dir, hash string, mi *metainfo.MetaInfo, onRead func(string, int64)) error {
	_, err := verifyStorage(ctx, dir, payload.Root(hash), mi, nil, onRead, false)
	return err
}

func verifyStorage(ctx context.Context, dir, rootPath string, mi *metainfo.MetaInfo, overrides map[string]string, onRead func(string, int64), readOnly bool) (*StorageVerification, error) {
	return verifyStorageLayout(ctx, dir, payload.Layout{Version: payload.NamedLayoutVersion, Root: rootPath}, mi, overrides, onRead, readOnly)
}

func VerifyReadOnlyLayout(ctx context.Context, dir string, layout payload.Layout, mi *metainfo.MetaInfo, overrides map[string]string, onRead func(string, int64)) (*StorageVerification, error) {
	if err := layout.Validate(); err != nil {
		return nil, err
	}
	return verifyStorageLayout(ctx, dir, layout, mi, overrides, onRead, true)
}

func verifyStorageLayout(ctx context.Context, dir string, layout payload.Layout, mi *metainfo.MetaInfo, overrides map[string]string, onRead func(string, int64), readOnly bool) (*StorageVerification, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	opts := storage.NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, NoCompletionRename: true, IOContext: ctx, ReadOnly: readOnly, IORead: onRead, PieceCompletion: storage.NewMapPieceCompletion(),
		TorrentDirMaker: func(base string, _ *metainfo.Info, _ metainfo.Hash) string {
			return filepath.Join(base, layout.Root)
		},
		TorrentPathResolver: func(base string, info *metainfo.Info, _ metainfo.Hash) (string, []string, error) {
			var paths []string
			for _, f := range info.UpvertedFiles() {
				path, err := layout.Relative(payload.Path(info, &f))
				if err != nil {
					return "", nil, err
				}
				if replacement, ok := overrides[path]; ok {
					path = replacement
				}
				paths = append(paths, path)
			}
			return filepath.Join(base, layout.Root), paths, nil
		},
	}
	if readOnly {
		opts.UsePartFiles = g.Some(false)
	}
	backend := storage.NewFileOpts(opts)
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
	cfg.HashBudget = sharedVerificationBudget
	cfg.VerificationBackground = true
	client, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		return nil, err
	}
	spec.DisallowDataDownload = true
	spec.DisableNetworking = true
	spec.DisallowDataUpload = true
	spec.Trackers = nil
	spec.Webseeds = nil
	spec.Sources = nil
	spec.PeerAddrs = nil
	spec.DhtNodes = nil
	t, _, err := client.AddTorrentSpec(spec)
	if err != nil {
		return nil, err
	}
	if err := t.VerifyDataContext(ctx); err != nil {
		return nil, err
	}
	result := &StorageVerification{}
	result.TotalPieces = t.Info().NumPieces()
	for index := 0; index < result.TotalPieces; index++ {
		if t.PieceState(index).Complete {
			result.ValidPieces++
		}
	}
	for _, f := range t.Files() {
		state := FileVerification{}
		if f.Length() > 0 {
			begin := int(f.Offset() / t.Info().PieceLength)
			end := int((f.Offset() + f.Length() + t.Info().PieceLength - 1) / t.Info().PieceLength)
			for i := begin; i < end; i++ {
				state.TotalPieces++
				if t.PieceState(i).Complete {
					state.ValidPieces++
				}
			}
		}
		result.Files = append(result.Files, state)
	}
	return result, nil
}

func (e *Engine) RunStorageTask(fn func(context.Context)) bool {
	return e.StartTask(fn)
}

var ErrMigrationDiskFull = errors.New("insufficient free space for safe storage operation")

func (e *Engine) CheckMigrationSpace(path string, bytes uint64) error {
	free, err := GetFreeSpace(path)
	if err != nil {
		return err
	}
	reserve := e.minimumDiskBytes()
	if free < reserve || bytes > free-reserve {
		return ErrMigrationDiskFull
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
