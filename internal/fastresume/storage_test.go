package fastresume

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/merkle"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

func storageFixture(t *testing.T, v2 bool) (Identity, *metainfo.MetaInfo, *metainfo.Info, storage.FileCheckpoint) {
	t.Helper()
	info := &metainfo.Info{Name: "film", PieceLength: 16384, Files: []metainfo.FileInfo{{Path: []string{"video"}, Length: 32768}}}
	mi := &metainfo.MetaInfo{}
	if v2 {
		info.MetaVersion, info.Files = 2, nil
		data := bytes.Repeat([]byte{1}, 32768)
		h := merkle.NewHash()
		h.Write(data)
		root := string(h.Sum(nil))
		info.FileTree = metainfo.FileTree{Dir: map[string]metainfo.FileTree{"video": {File: metainfo.FileTreeFile{Length: 32768, PiecesRoot: root}}}}
		h = merkle.NewHash()
		h.Write(data[:16384])
		leaf := string(h.Sum(nil))
		mi.PieceLayers = map[string]string{root: leaf + leaf}
	} else {
		h := sha1.Sum(bytes.Repeat([]byte{1}, 16384))
		info.Pieces = append(append([]byte{}, h[:]...), h[:]...)
	}
	var err error
	mi.InfoBytes, err = bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{RecordID: "record", StorageID: "storage", MappingRevision: 1, LayoutVersion: 2, Root: filepath.Join("payload", "film"), Prefix: "film", VolumeID: "volume"}
	native := storage.FileCheckpoint{InfoHash: canonicalStorageHash(mi, info), Root: id.Root, PieceLength: 16384, PieceCount: 2, ExtentLength: 32768, Complete: []byte{3}, Files: []storage.CheckpointFile{{Path: "video", Length: 32768, Present: true, Identity: "inode", Size: 32768, ModifiedNS: 100}}}
	return id, mi, info, native
}

func TestStorageConversion(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		id, mi, info, native := storageFixture(t, v2)
		saved, err := FromStorage(id, mi, info, 1, 1000, native)
		if err != nil {
			t.Fatal(v2, err)
		}
		native.Complete[0] = 0
		claims, err := StorageClaims(id, mi, info, saved, []byte{2})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(claims.Complete, []byte{1}) || saved.Pieces[0] != 3 || claims.Files[0] != native.Files[0] || claims.InfoHash != native.InfoHash {
			t.Fatal("conversion lost binding or mutated saved bitmap")
		}
		claims.Complete[0] = 0
		if saved.Pieces[0] != 3 {
			t.Fatal("claims alias saved bitmap")
		}
		if _, err := StorageClaims(id, mi, info, saved, []byte{128}); !errors.Is(err, ErrInvalid) {
			t.Fatal("accepted invalid revocation bitmap", err)
		}
		id.VolumeID = "other"
		if _, err := StorageClaims(id, mi, info, saved, []byte{0}); !errors.Is(err, ErrInvalid) {
			t.Fatal("accepted changed volume", err)
		}
	}
}

func TestStorageConversionCatalogGolden(t *testing.T) {
	id, mi, info, native := storageFixture(t, false)
	saved, err := FromStorage(id, mi, info, 1, 1000, native)
	if err != nil {
		t.Fatal(err)
	}
	// Independently encoded in Python with hashlib and struct.pack('<q', n).
	// This binds the catalog domain, byte order, string length and field order.
	if saved.Identity.MetainfoDigest != "3f7a078f1342057580c6ecd169a657c950209c34e4136b4fe5a7446aaa17d27e" || saved.Identity.CatalogDigest != "b2d0f898a5d803d6f06ffc302ec9e939dbf4de6259048d1577cf63b9f1d57a70" {
		t.Fatal("catalog format changed", saved.Identity)
	}
}

func TestStorageConversionRejectsMismatch(t *testing.T) {
	for _, change := range []string{"hash", "root", "geometry", "path", "offset", "length", "files", "bitmap", "prefix", "layers-missing", "layers-corrupt", "piece-budget", "file-budget", "v2-scratch-budget", "metadata", "catalog"} {
		t.Run(change, func(t *testing.T) {
			id, mi, info, native := storageFixture(t, true)
			saved, err := FromStorage(id, mi, info, 1, 1000, native)
			if err != nil {
				t.Fatal(err)
			}
			want := ErrInvalid
			switch change {
			case "hash":
				native.InfoHash[0] ^= 1
			case "root":
				native.Root = "other"
			case "geometry":
				native.PieceLength *= 2
			case "path":
				native.Files[0].Path = "other"
			case "offset":
				native.Files[0].Offset++
			case "length":
				native.Files[0].Length--
			case "files":
				native.Files = nil
			case "bitmap":
				native.Complete = []byte{128}
			case "prefix":
				id.Prefix = "other"
			case "layers-missing":
				mi.PieceLayers = nil
			case "layers-corrupt":
				for key := range mi.PieceLayers {
					mi.PieceLayers[key] = string(make([]byte, 64))
				}
			case "piece-budget":
				info.FileTree.Dir["video"] = metainfo.FileTree{File: metainfo.FileTreeFile{Length: int64(MaxPieces+1) * 16384}}
				want = ErrBudget
			case "file-budget":
				info.MetaVersion = 0
				info.Files = make([]metainfo.FileInfo, MaxFiles+1)
				for i := range info.Files {
					info.Files[i].Path = []string{"video"}
				}
				want = ErrBudget
			case "v2-scratch-budget":
				info.PieceLength = 128 << 20
				want = ErrBudget
			case "metadata":
				mi.InfoBytes = append(bytes.Clone(mi.InfoBytes), 'e')
			case "catalog":
				saved.Identity.CatalogDigest = saved.Identity.MetainfoDigest
			}
			if change == "catalog" {
				_, err = StorageClaims(id, mi, info, saved, []byte{0})
			} else {
				_, err = FromStorage(id, mi, info, 2, 1001, native)
			}
			if !errors.Is(err, want) {
				t.Fatalf("got %v, want %v", err, want)
			}
		})
	}
}
