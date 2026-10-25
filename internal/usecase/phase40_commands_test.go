package usecase

import (
	"bytes"
	"errors"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"torrent-stream-hub/internal/models"
)

func TestPhase40DuplicateMetadataAppliesOneRevision(t *testing.T) {
	for _, temporary := range []bool{false, true} {
		uc, repo, cleanup := setupTorrentUseCase(t)
		func() {
			defer cleanup()
			initial := temporaryFile(t, uc, "duplicate-metadata", false)
			if !temporary {
				if err := uc.MakePermanent(initial.Hash); err != nil {
					t.Fatal(err)
				}
				var err error
				initial, err = uc.GetTorrent(initial.Hash)
				if err != nil {
					t.Fatal(err)
				}
			}
			magnet := "magnet:?xt=urn:btih:" + initial.Hash
			got, err := uc.AddMagnetWithMetadata(magnet, TorrentMetadata{Title: "new title"})
			if err != nil {
				t.Fatal(err)
			}
			if got.Control.DesiredRevision != initial.Control.DesiredRevision+1 || got.Control.ReconcilePending || got.Title != "new title" || got.IsTemporary {
				t.Fatalf("unapplied composite command: %+v", got)
			}
			if _, err := capturePersistableRuntime(uc.engine, repo, got.Hash); err != nil {
				t.Fatal(err)
			}
			again, err := uc.AddMagnetWithMetadata(magnet, TorrentMetadata{Title: "new title"})
			if err != nil {
				t.Fatal(err)
			}
			if again.Control.DesiredRevision != got.Control.DesiredRevision || again.Control.ReconcilePending {
				t.Fatal("no-op changed revision", again.Control)
			}
			// An unapplied earlier Pause cannot be acknowledged by a metadata no-op.
			i, err := repo.GetIntent(got.Hash)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repo.CommitRunIntent(i.Version, models.RunPaused); err != nil {
				t.Fatal(err)
			}
			again, err = uc.AddMagnetWithMetadata(magnet, TorrentMetadata{Title: "new title"})
			if err != nil {
				t.Fatal(err)
			}
			if !again.Control.ReconcilePending || again.Control.AppliedRevision != got.Control.AppliedRevision {
				t.Fatal("no-op acknowledged earlier Pause", again.Control)
			}
		}()
	}
}

func TestPhase40V2MultiFileCatalog(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	mi := migrationMetainfo(t, "v2-multi", make([]byte, 1024), true, "v2")
	info, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	info.FileTree.Dir["second.bin"] = info.FileTree.Dir["video.bin"]
	mi.InfoBytes, err = bencode.Marshal(&info)
	if err != nil {
		t.Fatal(err)
	}
	var input bytes.Buffer
	if err := mi.Write(&input); err != nil {
		t.Fatal(err)
	}
	got, err := uc.AddTorrentFile(&input)
	if err != nil {
		t.Fatal(err)
	}
	if got.MetadataFormat != "v2" || got.Size != 2048 || len(got.Files) != 2 || got.Files[1].Offset != 16384 {
		t.Fatalf("invalid v2 catalog: %+v", got)
	}
	if _, err := capturePersistableRuntime(uc.engine, repo, got.Hash); err != nil {
		t.Fatal(err)
	}
	if err := uc.Pause(got.Hash); err != nil {
		t.Fatal(err)
	}
	if err := uc.engine.Delete(got.Hash); err != nil {
		t.Fatal(err)
	}
	if err := uc.RestoreTorrents(); err != nil {
		t.Fatal(err)
	}
	saved, err := repo.GetTorrent(got.Hash)
	if err != nil || saved.MetadataFormat != "v2" || saved.Files[1].Offset != 16384 || saved.Control.DesiredRun != models.RunPaused {
		t.Fatal(saved, err)
	}
}

func TestPrivateSafeFilePolicySurvivesRestore(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	mi := migrationMetainfo(t, "safe-file", []byte("payload"), false, "v1")
	var encoded bytes.Buffer
	if err := mi.Write(&encoded); err != nil {
		t.Fatal(err)
	}
	got, err := uc.AddTorrentFileWithPolicy(&encoded, TorrentMetadata{PrivateSafe: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := repo.GetTorrent(got.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(saved.SourceURI, "x.hub-private=1") {
		t.Fatal("policy not persisted", saved.SourceURI)
	}
	if uc.engine.GetTorrent(got.Hash) == nil {
		t.Fatal("runtime missing")
	}
	tor, err := uc.engine.GetTorrentFile(got.Hash, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tor.Torrent().PublicDiscoveryAllowed() {
		t.Fatal("file add enabled public discovery")
	}
	if err := uc.engine.Delete(got.Hash); err != nil {
		t.Fatal(err)
	}
	if err := uc.RestoreTorrents(); err != nil {
		t.Fatal(err)
	}
	tor, err = uc.engine.GetTorrentFile(got.Hash, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tor.Torrent().PublicDiscoveryAllowed() {
		t.Fatal("restore lost private-safe policy")
	}
}

func TestPrivateSafeCannotUpgradePublicHybridAlias(t *testing.T) {
	uc, _, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	mi := migrationMetainfo(t, "public-hybrid", bytes.Repeat([]byte("h"), 1024), true, "hybrid")
	spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		t.Fatal(err)
	}
	magnet := metainfo.MagnetV2{}
	magnet.V2InfoHash = spec.InfoHashV2
	got, err := uc.AddMagnet(magnet.String())
	if err != nil {
		t.Fatal(err)
	}
	raw := uc.engine.GetRawTorrent(got.Hash)
	supplyNamedMetadata(t, raw, mi)
	var encoded bytes.Buffer
	if err := mi.Write(&encoded); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.AddTorrentFileWithPolicy(&encoded, TorrentMetadata{PrivateSafe: true}, false); !errors.Is(err, ErrStorageConflict) {
		t.Fatal("file changed policy without durable source", err)
	}
	if _, err := uc.AddMagnetWithMetadata("magnet:?xt=urn:btih:"+spec.InfoHash.HexString(), TorrentMetadata{PrivateSafe: true}); !errors.Is(err, ErrStorageConflict) {
		t.Fatal("alias bypassed private-safe admission", err)
	}
	if !raw.PublicDiscoveryAllowed() {
		t.Fatal("rejected command mutated runtime")
	}
}
