package usecase

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"torrent-stream-hub/internal/fastresume"
	"torrent-stream-hub/internal/repository"
)

// resumeOpenSource is valid only inside ONE tracked library backend open. The
// library supplies its torrent closedCtx, validates the info hash, joins this
// call on Drop/Close and rechecks closed state before publishing the backend.
// RuntimeBinding is deliberately not looked up by hash to invent an attachment:
// on initial restore the managed runtime has not been published yet. Existing
// namespace commands retain their reservations and must Drop/join before effects;
// journal/mapping/record transitions also fence the durable token atomically.
func (uc *TorrentUseCase) resumeOpenSource(openCtx context.Context, mapping *repository.StorageMapping, record string, info *metainfo.Info) (repository.ResumeSource, error) {
	var source repository.ResumeSource
	m := *mapping
	intent, err := uc.repo.GetIntent(m.Hash)
	if err != nil {
		return source, err
	}
	if intent == nil || intent.Version.RecordID != record {
		return source, ErrStorageConflict
	}
	metadata, err := uc.resumeOpenMetadata(m.Hash, info)
	if err != nil {
		return source, err
	}
	volume := uc.resumeVolumeToken(m.Root)
	expectedRuntime, expectedFence := uc.engine.RuntimeBinding(m.Hash)
	source = repository.ResumeSource{
		Identity: fastresume.Identity{RecordID: record, StorageID: m.StorageID, MappingRevision: m.Revision, LayoutVersion: m.Version, Root: m.Root, Prefix: m.Prefix, VolumeID: volume},
		Metadata: metadata, Info: info,
	}
	source.Authorize = func(ctx context.Context) error {
		if err := openCtx.Err(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		currentRuntime, currentFence := uc.engine.RuntimeBinding(m.Hash)
		if currentRuntime != expectedRuntime || currentFence != expectedFence {
			return ErrStorageConflict
		}
		if currentRuntime != nil {
			if currentFence.RecordID != record || currentFence.Attachment == "" {
				return ErrStorageConflict
			}
			live, err := uc.engine.RuntimeSafetyBlocks(currentRuntime, m.Hash)
			if err != nil {
				return err
			}
			if len(live) != 0 {
				return ErrStorageConflict
			}
		}
		current, err := uc.repo.GetIntent(m.Hash)
		if err != nil {
			return err
		}
		if current == nil || current.Version != intent.Version {
			return ErrStorageConflict
		}
		if err := uc.repo.CheckResumeAdmission(ctx, &m, record); err != nil {
			return err
		}
		blocks, err := uc.repo.ActivationBlocks(intent.Version)
		if err != nil {
			return err
		}
		disk, err := uc.repo.DownloadBlock(m.Hash)
		if err != nil {
			return err
		}
		if len(blocks) != 0 || disk != "" || volume == "" || uc.resumeVolumeToken(m.Root) != volume {
			return ErrStorageConflict
		}
		if err := uc.inventoryAdmitted(&m); err != nil {
			return err
		}
		return uc.checkVolume(m.Root)
	}
	return source, source.Authorize(openCtx)
}

func (uc *TorrentUseCase) resumeOpenMetadata(hash string, info *metainfo.Info) (*metainfo.MetaInfo, error) {
	f, err := os.Open(filepath.Join(uc.engine.ConfigDir(), "metainfo", hash+".torrent"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	files, err := resumeFileCount(info)
	if err != nil {
		return nil, err
	}
	count := int64(info.NumPieces())
	// Include bounded cached-metainfo decoding alongside the worst-case native
	// import, not just its bitmap. Oversize is ordinary verification fallback.
	if st.Size() < 0 || st.Size() > fastresume.MaxCheckpointBytes || count < 0 || count > fastresume.MaxPieces || files > fastresume.MaxFiles || count*256+int64(files)*2048+((count+7)/8)*8+st.Size()*4 > fastresume.RestoreMemoryBytes {
		return nil, fastresume.ErrBudget
	}
	var metadata metainfo.MetaInfo
	d := bencode.NewDecoder(io.LimitReader(f, st.Size()+1))
	d.MaxStrLen = bencode.MaxStrLen(st.Size() + 1)
	if err := d.Decode(&metadata); err != nil {
		return nil, err
	}
	if err := d.ReadEOF(); err != nil {
		return nil, err
	}
	v1 := metainfo.Hash(sha1.Sum(metadata.InfoBytes))
	v2 := sha256.Sum256(metadata.InfoBytes)
	var short metainfo.Hash
	copy(short[:], v2[:])
	if v1.HexString() != hash && short.HexString() != hash {
		return nil, errors.New("resume metainfo hash mismatch")
	}
	return &metadata, nil
}
