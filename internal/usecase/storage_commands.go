package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"
)

func decodeLocationJournal(v *repository.RecoveryRecord) (*storageLocationJournal, error) {
	j := new(storageLocationJournal)
	if err := json.Unmarshal([]byte(v.Journal), j); err != nil {
		return nil, err
	}
	if err := validateLocationJournal(v, j); err != nil {
		return nil, ErrStorageConflict
	}
	return j, nil
}

func (uc *TorrentUseCase) locationTransition(m *repository.StorageMapping, v *repository.RecoveryRecord, j *storageLocationJournal, stage string) error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	locationSummary(v, j)
	from := v.Stage
	v.Stage = stage
	v.Journal = string(data)
	if err := uc.repo.TransitionStorageOperation(m, v, from); err != nil {
		v.Stage = from
		return err
	}
	if from != stage {
		uc.storageStage(v.Hash, stage, nil, nil)
	}
	return uc.recoveryBoundary("location_" + stage)
}

func (uc *TorrentUseCase) wakeLocation() {
	select {
	case uc.locationWake <- struct{}{}:
	default:
	}
}

// StorageCommand serializes only admission and short journal transactions.
// The engine-owned worker survives disconnect and owns all slow filesystem work.
func (uc *TorrentUseCase) StorageCommand(ctx context.Context, hash string, cmd models.StorageCommand) (*models.StorageLocation, error) {
	if uc.engine.Context().Err() != nil {
		return nil, context.Canceled
	}
	release, err := uc.lockRecord(hash, ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	t, err := uc.repo.GetTorrent(hash)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, ErrTorrentNotFound
	}
	m, err := uc.repo.StorageMapping(hash)
	if err != nil {
		return nil, err
	}
	if t.Control == nil || t.Control.RecordID != cmd.RecordID || m == nil || m.StorageID != cmd.StorageID || t.DeletionMode != "" && cmd.Action != "reconcile_storage" {
		return nil, ErrStorageConflict
	}
	rec, err := uc.repo.Recovery(hash)
	if err != nil {
		return nil, err
	}
	// A repeated apply after commit acknowledges the same operation; it never
	// grants a fresh operation permission using an old mapping revision.
	idempotent := false
	if rec != nil && rec.RecordID == cmd.RecordID && rec.Version == cmd.Version && ((rec.Reason == models.StorageMove && cmd.Action == "apply_move") || (rec.Reason == models.StorageLocate && cmd.Action == "apply_location")) && (rec.Stage == "completed" || rec.Stage == "committed" || rec.Stage == "cleanup") {
		j, e := decodeLocationJournal(rec)
		idempotent = e == nil && cmd.MappingRevision == j.Source.Revision && m.Revision == j.Source.Revision+1 && m.Layout() == j.Target
	}
	if m.Revision != cmd.MappingRevision && !idempotent {
		return nil, ErrStorageConflict
	}
	if idempotent {
		return t.Storage, nil
	}
	if cmd.Action == "reconcile_storage" {
		mutation, err := uc.coordinator.tryAcquire("storage/mutation")
		if err != nil {
			return nil, ErrStorageConflict
		}
		defer mutation.release()
		// Enrollment must remain possible when an upgraded library contains
		// only interrupted journals. It must not reset those journals or admit
		// their payload; a separate retry/cancel still controls the operation.
		blocked := t.DeletionMode != "" || recoveryBlocks(rec) || !m.Ready()
		if err := uc.reconcileInventory(ctx, m, cmd.RecordID, cmd.Path, t.DeletionMode != "", blocked); err != nil {
			return nil, err
		}
		result, err := uc.repo.GetTorrent(hash)
		if err != nil {
			return nil, err
		}
		return result.Storage, nil
	}
	if cmd.Action == "plan_move" || cmd.Action == "analyze_location" {
		if !m.Ready() {
			return nil, ErrStorageConflict
		}
		info, err := uc.metadataStorageInfo(t)
		if err != nil {
			return nil, err
		}
		if info == nil {
			return nil, ErrMetadataPending
		}
		name := cmd.FolderName
		if name == "" {
			name = info.BestName()
		}
		target := payload.Layout{Version: payload.NamedLayoutVersion, Root: payload.NamedRoot(name, 1), Prefix: payload.MetadataPrefix(info)}
		if cmd.Action == "analyze_location" {
			target = payload.Layout{Version: payload.NamedLayoutVersion, Root: cmd.Path, Prefix: cmd.StripPrefix}
			if err := target.Validate(); err != nil {
				return nil, ErrStorageConflict
			}
			for _, f := range info.UpvertedFiles() {
				if _, err := target.File(info, &f); err != nil {
					return nil, ErrStorageConflict
				}
			}
		}
		if payload.Overlap(payload.CollisionKey(m.Root), payload.CollisionKey(target.Root)) {
			return nil, fmt.Errorf("%w: source and target overlap", ErrStorageConflict)
		}
		if rec != nil && (rec.Stage == "planning" || rec.Stage == "planned" || rec.Stage == "analyzing_location" || rec.Stage == "analyzed") {
			old, e := decodeLocationJournal(rec)
			if e == nil && old.Target == target && old.Source.Revision == m.Revision && (cmd.Version == rec.Version || cmd.Version == rec.Version-1) {
				return t.Storage, nil
			}
		}
		previous := int64(0)
		var alternatives []preservedAlternative
		if rec != nil {
			if rec.RecordID != cmd.RecordID {
				return nil, ErrStorageConflict
			}
			if rec.Reason != models.StorageMove && rec.Reason != models.StorageLocate && rec.Reason != models.StorageRecoveryComplete {
				return nil, ErrStorageConflict
			}
			if rec.Stage != "planned" && rec.Stage != "planning" && rec.Stage != "analyzed" && rec.Stage != "analyzing_location" && rec.Stage != "completed" && rec.Stage != "cancelled" && rec.Reason != models.StorageRecoveryComplete {
				return nil, ErrStorageConflict
			}
			if cmd.Version != rec.Version {
				return nil, ErrStorageConflict
			}
			previous = rec.Version
			old, e := decodeLegacyJournal(rec)
			if e != nil {
				return nil, e
			}
			alternatives = old.Alternatives
		} else if cmd.Version != 0 {
			return nil, ErrStorageConflict
		}

		v := &repository.RecoveryRecord{Hash: hash, RecordID: cmd.RecordID, Version: previous + 1, Reason: models.StorageMove, Stage: "planning"}
		if cmd.Action == "analyze_location" {
			v.Reason, v.Stage = models.StorageLocate, "analyzing_location"
		}
		j := &storageLocationJournal{Schema: 1, Source: m, Target: target, Staging: filepath.Join(filepath.Dir(target.Root), ".tsh-recovery", "moves", m.StorageID, fmt.Sprintf("%s-%d", cmd.RecordID, v.Version)), Alternatives: alternatives}
		data, err := json.Marshal(j)
		if err != nil {
			return nil, err
		}
		v.Journal = string(data)
		locationSummary(v, j)
		claims := []repository.StorageClaim{}
		for _, p := range []struct{ path, kind string }{{m.Root, "source"}, {target.Root, "target"}, {j.Staging, "staging"}, {j.Staging + ".owner", "staging"}} {
			claims = append(claims, repository.StorageClaim{StorageID: m.StorageID, RecordID: cmd.RecordID, Version: v.Version, Path: p.path, Kind: p.kind})
		}
		if err := uc.repo.CreateStorageOperation(ctx, m, v, previous, claims); err != nil {
			return nil, err
		}
	} else {
		if rec == nil || rec.RecordID != cmd.RecordID || rec.Version != cmd.Version || (rec.Reason != models.StorageMove && rec.Reason != models.StorageLocate) {
			return nil, ErrStorageConflict
		}
		j, err := decodeLocationJournal(rec)
		if err != nil {
			return nil, err
		}
		switch cmd.Action {
		case "apply_move", "apply_location":
			if cmd.Action == "apply_location" && !locationMatches(j.Analysis) {
				return nil, ErrStorageConflict
			}
			if (cmd.Action == "apply_move") != (rec.Reason == models.StorageMove) {
				return nil, ErrStorageConflict
			}
			if rec.Stage != "planned" && rec.Stage != "analyzed" {
				if rec.Stage == "completed" || rec.Stage == "queued" || rec.Stage == "waiting_readers" || rec.Stage == "prepared" || rec.Stage == "copying" || rec.Stage == "publishing" || rec.Stage == "published" || rec.Stage == "committed" || rec.Stage == "cleanup" {
					return t.Storage, nil
				}
				return nil, ErrStorageConflict
			}
			if rec.Error != "" {
				return nil, ErrStorageConflict
			}
			if err := uc.locationTransition(m, rec, j, "queued"); err != nil {
				return nil, err
			}
		case "retry_storage":
			if rec.Stage == "completed" || rec.Stage == "cancelled" {
				return t.Storage, nil
			}
			rec.Error = ""
			rec.FailureCode = ""
			if err := uc.locationTransition(m, rec, j, rec.Stage); err != nil {
				return nil, err
			}
		case "cancel_storage":
			switch rec.Stage {
			case "publishing", "published", "committed", "cleanup", "completed":
				return nil, ErrStorageConflict
			case "cancelled":
				return t.Storage, nil
			}
			rec.Error = ""
			rec.FailureCode = ""
			if err := uc.locationTransition(m, rec, j, "cancel_requested"); err != nil {
				return nil, err
			}
			uc.cancelLocation(hash, cmd.RecordID, cmd.Version)
		default:
			return nil, fmt.Errorf("unsupported storage action")
		}
	}
	uc.wakeLocation()
	result, err := uc.repo.GetTorrent(hash)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, ErrTorrentNotFound
	}
	return result.Storage, nil
}

func locationMappingMatches(m *repository.StorageMapping, j *storageLocationJournal, stage string) bool {
	if m == nil || m.StorageID != j.Source.StorageID {
		return false
	}
	if stage == "committed" || stage == "cleanup" || stage == "completed" {
		return m.Revision == j.Source.Revision+1 && m.Layout() == j.Target
	}
	return m.Revision == j.Source.Revision && m.Layout() == j.Source.Layout()
}

var errLocationCancelled = errors.New("storage operation cancelled")

func locationSummary(v *repository.RecoveryRecord, j *storageLocationJournal) {
	if j.Analysis != nil {
		b, _ := json.Marshal(j.Analysis)
		v.LocationAnalysis = string(b)
	}
	v.LocationSource = j.Source.Root
	v.LocationTarget = j.Target.Root
	v.LocationMethod = j.Method
	v.LocationExtraBytes = j.ExtraBytes
	v.LocationFreeBytes = j.FreeBytes
	v.LocationAlternatives = len(j.Alternatives)
	v.LocationAlternativeBytes = 0
	for _, a := range j.Alternatives {
		v.LocationAlternativeBytes += a.Size
	}
}
