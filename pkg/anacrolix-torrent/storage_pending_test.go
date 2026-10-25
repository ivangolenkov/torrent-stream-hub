package torrent

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

func TestPendingStorageKeepsValidatedPrivateMetadata(t *testing.T) {
	cl := newTestingClient(t)
	var prepared atomic.Bool
	dir := filepath.Join(t.TempDir(), "payload")
	backend := storage.NewFileOpts(storage.NewFileClientOpts{ClientBaseDir: dir,
		TorrentPathResolver: func(base string, _ *metainfo.Info, _ metainfo.Hash) (string, []string, error) {
			if !prepared.Load() {
				return "", nil, storage.ErrStoragePending
			}
			return base, []string{"empty"}, nil
		},
	})
	defer backend.Close()
	private := true
	encoded, err := bencode.Marshal(metainfo.Info{Name: "empty", PieceLength: 4, Private: &private})
	if err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: encoded}
	spec := new(TorrentSpec)
	spec.InfoHash = mi.HashInfoBytes()
	spec.Storage = backend
	spec.DisableNetworking = true
	spec.DisallowDataDownload = true
	tor, _, err := cl.AddTorrentSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := tor.SetInfoBytes(encoded); !errors.Is(err, storage.ErrStoragePending) {
		t.Fatal(err)
	}
	if tor.Info() != nil || tor.PublicDiscoveryAllowed() {
		t.Fatal("pending info opened storage or lost private policy")
	}
	if !bytes.Equal(tor.PendingStorageMetadata(), encoded) {
		t.Fatal("pending metadata lost")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("storage created before reservation", err)
	}
	prepared.Store(true)
	if err := tor.SetInfoBytes(encoded); err != nil {
		t.Fatal(err)
	}
	if tor.Info() == nil || len(tor.PendingStorageMetadata()) != 0 || tor.PublicDiscoveryAllowed() {
		t.Fatal("retry did not complete private storage")
	}
}
