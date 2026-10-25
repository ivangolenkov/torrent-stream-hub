package usecase

import (
	"bytes"
	"context"
	"errors"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"os"
	"path/filepath"
	"testing"
	"time"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"
)

func supplyNamedMetadata(t *testing.T, raw *torrent.Torrent, mi *metainfo.MetaInfo) {
	t.Helper()
	if err := raw.SetInfoBytes(mi.InfoBytes); err != nil && !errors.Is(err, storage.ErrStoragePending) {
		t.Fatal(err)
	}
	select {
	case <-raw.GotInfo():
	case <-time.After(5 * time.Second):
		t.Fatal("metadata storage preparation did not finish")
	}
}

func TestUnknownStorageVersionNeverUsesSharedPaths(t *testing.T) {
	uc, repo, done := setupTorrentUseCase(t)
	defer done()
	tor := temporaryFile(t, uc, "unknown", false)
	if err := uc.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	m, _ := repo.StorageMapping(tor.Hash)
	m.Version = 999
	if err := repo.SaveStorageMapping(m); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.knownStoragePaths(tor); !errors.Is(err, ErrStorageConflict) {
		t.Fatal("unknown layout fell back", err)
	}
	if _, err := uc.snapshotStoragePaths(repository.OwnershipRecord{Torrent: tor, Mapping: m}); !errors.Is(err, ErrStorageConflict) {
		t.Fatal("ownership accepted unknown layout", err)
	}
	if err := uc.Delete(tor.Hash, true); !errors.Is(err, ErrStorageConflict) {
		t.Fatal("delete accepted unknown layout", err)
	}
}

func TestNamedStorageCollisionPrefixAndPersistedChoice(t *testing.T) {
	uc, repo, done := setupTorrentUseCase(t)
	defer done()
	foreign := filepath.Join(uc.engine.DownloadDir(), "payload", "SERIES")
	if err := os.MkdirAll(foreign, 0700); err != nil {
		t.Fatal(err)
	}
	for i, version := range []string{"v1", "v2", "hybrid"} {
		mi := migrationMetainfo(t, "Series", bytes.Repeat([]byte{byte(i + 1)}, 1024), true, version)
		var b bytes.Buffer
		mi.Write(&b)
		tor, err := uc.AddTorrentFile(&b)
		if err != nil {
			t.Fatal(err)
		}
		m, err := repo.StorageMapping(tor.Hash)
		if err != nil {
			t.Fatal(err)
		}
		if m.Root != payload.NamedRoot("Series", i+2) || m.Prefix != "Series" || m.Version != 2 || m.StorageID == "" {
			t.Fatalf("layout: %+v", m)
		}
		info, _ := mi.UnmarshalInfo()
		paths, err := uc.knownStoragePaths(tor)
		if err != nil {
			t.Fatal(err)
		}
		for j, f := range info.UpvertedFiles() {
			want, _ := m.Layout().File(&info, &f)
			if paths[j] != want || filepath.Dir(want) != m.Root {
				t.Fatalf("resolver mismatch %q != %q", paths[j], want)
			}
		}
		if err := uc.Pause(tor.Hash); err != nil {
			t.Fatal(err)
		}
		if _, err := uc.UpdateMetadata(tor.Hash, TorrentMetadata{Title: "Other title"}); err != nil {
			t.Fatal(err)
		}
		if err := uc.engine.Delete(tor.Hash); err != nil {
			t.Fatal(err)
		}
		if ok, err := uc.restorePersistedTorrent(context.Background(), tor.Hash); err != nil || !ok {
			t.Fatal(ok, err)
		}
		after, _ := repo.StorageMapping(tor.Hash)
		if *after != *m {
			t.Fatalf("restore changed location: %+v -> %+v", m, after)
		}
		if got := uc.engine.GetTorrent(tor.Hash); got.State != models.StatePaused {
			t.Fatal("restore resumed", got.State)
		}
	}
}

func TestMagnetHasNoWritableRootBeforeMetadata(t *testing.T) {
	uc, repo, done := setupTorrentUseCase(t)
	defer done()
	mi := migrationMetainfo(t, "metadata-name", bytes.Repeat([]byte("x"), 1024), true, "hybrid")
	spec, _ := torrent.TorrentSpecFromMetaInfoErr(mi)
	magnet := metainfo.MagnetV2{DisplayName: "../../unsafe"}
	magnet.V2InfoHash = spec.InfoHashV2
	tor, err := uc.AddMagnetWithMetadata(magnet.String(), TorrentMetadata{FolderName: "My Folder"})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := repo.StorageMapping(tor.Hash)
	if m.Root != "" || m.State != "reserved" {
		t.Fatalf("early storage: %+v", m)
	}
	if _, err := os.Stat(filepath.Join(uc.engine.DownloadDir(), "payload")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("metadata-only add created payload", err)
	}
	raw := uc.engine.GetRawTorrent(tor.Hash)
	supplyNamedMetadata(t, raw, mi)
	m, _ = repo.StorageMapping(tor.Hash)
	alias, _ := repo.StorageMapping(spec.InfoHash.HexString())
	if !m.Ready() || m.Root != payload.NamedRoot("My Folder", 1) || alias == nil || m.StorageID != alias.StorageID || m.Revision != alias.Revision {
		t.Fatalf("alias layout: %+v %+v", m, alias)
	}
}
