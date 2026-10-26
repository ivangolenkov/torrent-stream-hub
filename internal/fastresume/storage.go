package fastresume

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"math"
	"sort"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"torrent-stream-hub/internal/payload"
)

// Callers provide the immutable Info corresponding to validated InfoBytes from
// the live canonical torrent, not an unvalidated HTTP object. No payload I/O or
// library/repository callbacks occur here; native evidence comes from storage.
func catalogIdentity(base Identity, mi *metainfo.MetaInfo, info *metainfo.Info) (Identity, []File, error) {
	if mi == nil || info == nil || len(mi.InfoBytes) == 0 || info.PieceLength <= 0 {
		return base, nil, ErrInvalid
	}
	layout := payload.Layout{Version: base.LayoutVersion, Root: base.Root, Prefix: base.Prefix}
	if err := layout.Validate(); err != nil {
		return base, nil, ErrInvalid
	}
	var files []File
	var extent int64
	pathBytes := 0
	for fi := range info.UpvertedFilesIter() {
		if len(files) >= MaxFiles {
			return base, nil, ErrBudget
		}
		path, err := layout.Relative(payload.Path(info, &fi))
		if err != nil {
			return base, nil, ErrInvalid
		}
		pathBytes += len(path)
		if pathBytes > MaxPathBytes || len(path) > MaxPathLength {
			return base, nil, ErrBudget
		}
		if fi.TorrentOffset < 0 || fi.Length < 0 || fi.Length > math.MaxInt64-fi.TorrentOffset {
			return base, nil, ErrInvalid
		}
		files = append(files, File{Path: path, Offset: fi.TorrentOffset, Length: fi.Length})
		extent = max(extent, fi.TorrentOffset+fi.Length)
	}
	base.PieceLength, base.PieceCount, base.ExtentLength = info.PieceLength, info.NumPieces(), extent
	metaDigest := sha256.Sum256(mi.InfoBytes)
	base.MetainfoDigest = fmt.Sprintf("%x", metaDigest)
	if len(files) == 0 || base.PieceCount < 0 || base.PieceCount > MaxPieces {
		return base, nil, ErrBudget
	}
	h := sha256.New()
	h.Write([]byte("TSH-file-catalog-v1"))
	h.Write(metaDigest[:])
	for _, n := range []int64{base.PieceLength, int64(base.PieceCount), extent, int64(len(files))} {
		catalogNumber(h, n)
	}
	for _, f := range files {
		catalogText(h, f.Path)
		catalogNumber(h, f.Offset)
		catalogNumber(h, f.Length)
	}
	// The v2 padding helper allocates one hash per 16 KiB block. Limit its
	// scratch to 128 KiB and compact layers to 4 MiB before invoking validation.
	layers := make(map[string]string)
	if info.HasV2() {
		if info.PieceLength < 16384 || info.PieceLength > 64<<20 || info.PieceLength&(info.PieceLength-1) != 0 {
			return base, nil, ErrBudget
		}
		total := 0
		for fi := range info.UpvertedFilesIter() {
			if fi.Length > 0 && !fi.PiecesRoot.Ok {
				return base, nil, ErrInvalid
			}
			if fi.Length <= info.PieceLength {
				continue
			}
			if !fi.PiecesRoot.Ok {
				return base, nil, ErrInvalid
			}
			key := string(fi.PiecesRoot.Value[:])
			raw, ok := mi.PieceLayers[key]
			count := fi.Length / info.PieceLength
			if fi.Length%info.PieceLength != 0 {
				count++
			}
			if !ok || int64(len(raw)) != count*32 {
				return base, nil, ErrInvalid
			}
			if _, exists := layers[key]; !exists {
				total += len(raw)
			}
			if total > MaxCheckpointBytes {
				return base, nil, ErrBudget
			}
			layers[key] = raw
		}
		if err := metainfo.ValidatePieceLayers(layers, &info.FileTree, info.PieceLength); err != nil {
			return base, nil, fmt.Errorf("%w: v2 piece layers", ErrInvalid)
		}
	}
	keys := make([]string, 0, len(layers))
	for key := range layers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	catalogNumber(h, int64(len(keys)))
	for _, key := range keys {
		catalogText(h, key)
		catalogText(h, layers[key])
	}
	base.CatalogDigest = fmt.Sprintf("%x", h.Sum(nil))
	if err := base.validate(); err != nil {
		return base, nil, err
	}
	return base, files, nil
}

func catalogNumber(h hash.Hash, n int64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(n))
	h.Write(b[:])
}
func catalogText(h hash.Hash, s string) {
	catalogNumber(h, int64(len(s)))
	for len(s) > 0 {
		n := min(len(s), ScratchBytes)
		h.Write([]byte(s[:n]))
		s = s[n:]
	}
}
func canonicalStorageHash(mi *metainfo.MetaInfo, info *metainfo.Info) metainfo.Hash {
	if info.HasV1() {
		return sha1.Sum(mi.InfoBytes)
	}
	digest := sha256.Sum256(mi.InfoBytes)
	var short metainfo.Hash
	copy(short[:], digest[:])
	return short
}

// FromStorage binds native post-flush evidence to canonical application identity.
// It must be called and committed inside WithFileCheckpoint's publish callback.
func FromStorage(base Identity, mi *metainfo.MetaInfo, info *metainfo.Info, generation, nowMS int64, native storage.FileCheckpoint) (*Checkpoint, error) {
	id, files, err := catalogIdentity(base, mi, info)
	if err != nil {
		return nil, err
	}
	if native.InfoHash != canonicalStorageHash(mi, info) || native.Root != id.Root || native.PieceLength != id.PieceLength || native.PieceCount != id.PieceCount || native.ExtentLength != id.ExtentLength || len(native.Files) != len(files) {
		return nil, ErrInvalid
	}
	for i, f := range native.Files {
		if f.Path != files[i].Path || f.Offset != files[i].Offset || f.Length != files[i].Length {
			return nil, ErrInvalid
		}
		files[i].Evidence = Evidence{Present: f.Present, Identity: f.Identity, Size: f.Size, ModifiedNS: f.ModifiedNS}
	}
	c := &Checkpoint{Schema: SchemaVersion, Identity: id, Generation: generation, CreatedAtMS: nowMS, Files: files, Pieces: bytes.Clone(native.Complete)}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// StorageClaims subtracts durable revocations and rejects metadata/mapping/layer
// mismatches. The backend must STILL issue and consume its one-open RestoreToken
// after rooted file checks and a final owner/generation/attachment authorization.
func StorageClaims(base Identity, mi *metainfo.MetaInfo, info *metainfo.Info, saved *Checkpoint, revoked []byte) (storage.FileCheckpoint, error) {
	var native storage.FileCheckpoint
	if err := saved.Validate(); err != nil {
		return native, err
	}
	expected, files, err := catalogIdentity(base, mi, info)
	if err != nil {
		return native, err
	}
	if expected != saved.Identity || len(files) != len(saved.Files) {
		return native, ErrInvalid
	}
	if err := ValidateBitmap(revoked, expected.PieceCount); err != nil {
		return native, err
	}
	native = storage.FileCheckpoint{InfoHash: canonicalStorageHash(mi, info), Root: expected.Root, PieceLength: expected.PieceLength, PieceCount: expected.PieceCount, ExtentLength: expected.ExtentLength, Complete: bytes.Clone(saved.Pieces)}
	for i := range native.Complete {
		native.Complete[i] &^= revoked[i]
	}
	for i, f := range saved.Files {
		if f.Path != files[i].Path || f.Offset != files[i].Offset || f.Length != files[i].Length {
			return storage.FileCheckpoint{}, ErrInvalid
		}
		e := f.Evidence
		native.Files = append(native.Files, storage.CheckpointFile{Path: f.Path, Offset: f.Offset, Length: f.Length, Present: e.Present, Identity: e.Identity, Size: e.Size, ModifiedNS: e.ModifiedNS})
	}
	return native, nil
}
