package usecase

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"
)

func (uc *TorrentUseCase) locationMetainfo(hash string) (*metainfo.MetaInfo, error) {
	mi, err := metainfo.LoadFromFile(filepath.Join(uc.engine.ConfigDir(), "metainfo", hash+".torrent"))
	if err != nil {
		return nil, err
	}
	spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		return nil, err
	}
	matches := spec.InfoHash.HexString() == hash
	if spec.InfoHashV2.Ok {
		matches = matches || spec.InfoHashV2.Value.ToShort().HexString() == hash
	}
	if !matches {
		return nil, ErrStorageConflict
	}
	return mi, nil
}

func (uc *TorrentUseCase) executeLocate(ctx context.Context, root *os.Root, m *repository.StorageMapping, v *repository.RecoveryRecord, j *storageLocationJournal) error {
	claim, err := uc.reserveOwnership(ctx, root, v.Hash, []string{j.Target.Root})
	if err != nil {
		return err
	}
	defer claim()
	for {
		if err := uc.checkLocationVolumes(j, true); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		switch v.Stage {
		case "analyzing_location":
			return uc.analyzeLocation(ctx, root, m, v, j)
		case "analyzed", "completed", "cancelled", "cancel_requested":
			return nil
		case "queued":
			if err := uc.locationTransition(m, v, j, "waiting_readers"); err != nil {
				return err
			}
		case "waiting_readers":
			if err := uc.waitLocationReaders(ctx, v.Hash); err != nil {
				return err
			}
			if err := uc.engine.Delete(v.Hash); err != nil {
				return err
			}
			if err := uc.locationTransition(m, v, j, "prepared"); err != nil {
				return err
			}
		case "prepared":
			if !locationMatches(j.Analysis) {
				return ErrStorageConflict
			}
			if err := validateStorageTree(ctx, root, j.Target.Root, j.CandidateTree, uc.storageIOBytes(v.Hash)); err != nil {
				return err
			}
			if err := uc.locationTransition(m, v, j, "publishing"); err != nil {
				return err
			}
		case "publishing":
			if err := uc.locationTransition(m, v, j, "published"); err != nil {
				return err
			}
		case "published":
			if err := validateStorageTree(ctx, root, j.Target.Root, j.CandidateTree, uc.storageIOBytes(v.Hash)); err != nil {
				return err
			}
			if err := uc.repo.CommitStorageOperation(ctx, j.Source, v, j.Target, true); err != nil {
				return err
			}
			v.Stage = "committed"
			if err := uc.recoveryBoundary("location_mapping_committed"); err != nil {
				return err
			}
			m, err = uc.repo.StorageMapping(v.Hash)
			if err != nil {
				return err
			}
		case "committed":
			// Locate never cleans the original, including an absent source. Its
			// retained reservation survives later remount/reappearance and Delete.
			v.Stage = "completed"
			if err := uc.repo.FinishStorageOperation(m, v, "committed", []string{j.Source.Root}); err != nil {
				return err
			}
			if err := uc.recoveryBoundary("location_completed"); err != nil {
				return err
			}
			return uc.restoreAfterLocation(ctx, v)
		default:
			return ErrStorageConflict
		}
	}
}

func (uc *TorrentUseCase) storageIOBytes(hash string) func(int64) {
	return func(n int64) { uc.storageBytes(hash, n) }
}

func locationMatches(a *models.LocationAnalysis) bool {
	return a != nil && (a.ValidPieces > 0 || (a.TotalPieces == 0 && a.FoundFiles > 0 && a.MissingFiles == 0))
}

func (uc *TorrentUseCase) analyzeLocation(ctx context.Context, root *os.Root, m *repository.StorageMapping, v *repository.RecoveryRecord, j *storageLocationJournal) error {
	mi, err := uc.locationMetainfo(v.Hash)
	if err != nil {
		return err
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return err
	}
	tree, err := snapshotStorageTree(ctx, root, j.Target.Root, uc.storageIOBytes(v.Hash))
	if err != nil {
		return err
	}
	if tree.Identity == "" {
		return os.ErrNotExist
	}
	result, err := engine.VerifyReadOnlyLayout(ctx, uc.engine.DownloadDir(), j.Target, mi, nil, uc.storageIORead(v.Hash))
	if err != nil {
		return err
	}
	analysis := &models.LocationAnalysis{ValidPieces: result.ValidPieces, TotalPieces: result.TotalPieces,
		Limitations: []string{"Analysis does not grant runtime readiness. Shared boundary pieces require every contributing file. Applying preserves the old location and leaves the torrent paused."}}
	for _, file := range info.UpvertedFiles() {
		path, err := j.Target.File(&info, &file)
		if err != nil {
			return err
		}
		f, err := payload.Open(root, path)
		if errors.Is(err, os.ErrNotExist) {
			analysis.MissingFiles++
			continue
		}
		if err != nil {
			return err
		}
		st, statErr := f.Stat()
		f.Close()
		if statErr != nil {
			return statErr
		}
		if file.Length == 0 && st.Size() != 0 {
			analysis.MissingFiles++
		} else {
			analysis.FoundFiles++
		}
	}
	if err := validateStorageTree(ctx, root, j.Target.Root, tree, nil); err != nil {
		return err
	}
	j.CandidateTree, j.Analysis, j.Method = tree, analysis, "locate"
	return uc.locationTransition(m, v, j, "analyzed")
}
