package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"
)

type recoveryFingerprint struct {
	Identity string
	Size     int64
	Modified int64
	Digest   string
}

var ErrRecoveryCandidateChanged = errors.New("candidate changed; analyze again before applying")

type preservedAlternative struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}
type legacyRecoveryJournal struct {
	MetadataPath string                 `json:"metadata_path,omitempty"`
	Analysis     *models.LegacyAnalysis `json:"analysis,omitempty"`
	Final        recoveryFingerprint    `json:"final_fingerprint"`
	Part         recoveryFingerprint    `json:"part_fingerprint"`
	Choice       string                 `json:"choice,omitempty"`
	Backup       string                 `json:"backup,omitempty"`
	Alternatives []preservedAlternative `json:"alternatives,omitempty"`
}

func recoveryBlocks(v *repository.RecoveryRecord) bool {
	if v == nil {
		return false
	}
	if v.Reason == models.StorageMove || v.Reason == models.StorageLocate {
		return v.Stage != "planning" && v.Stage != "planned" && v.Stage != "analyzing_location" && v.Stage != "analyzed" && v.Stage != "completed" && v.Stage != "cancelled"
	}
	if v.Reason == models.StorageDeletionCancelled || v.Stage == "cancel_requested" {
		return true
	}
	switch v.Stage {
	case "analyzing", "analyzed", "prepared", "preserved", "published", "verifying":
		return true
	}
	return false
}
func decodeLegacyJournal(v *repository.RecoveryRecord) (legacyRecoveryJournal, error) {
	var j legacyRecoveryJournal
	if v != nil && v.Journal != "" {
		err := json.Unmarshal([]byte(v.Journal), &j)
		return j, err
	}
	return j, nil
}
func (uc *TorrentUseCase) saveLegacyJournal(v *repository.RecoveryRecord, j *legacyRecoveryJournal, stage string) error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	v.Journal = string(data)
	v.Stage = stage
	return uc.repo.SaveRecovery(v)
}
func (uc *TorrentUseCase) recoveryBoundary(stage string) error {
	if uc.recoveryStep != nil {
		return uc.recoveryStep(stage)
	}
	return nil
}

func fingerprintRecovery(ctx context.Context, root *os.Root, path string, observe func(int64)) (recoveryFingerprint, error) {
	var result recoveryFingerprint
	f, err := payload.Open(root, path)
	if err != nil {
		return result, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return result, err
	}
	result.Identity, err = storage.FileIdentity(f)
	if err != nil {
		return result, err
	}
	h := sha256.New()
	n, err := io.CopyBuffer(h, observedReader{ctx: ctx, read: f.Read, observe: observe}, make([]byte, 128<<10))
	if err != nil {
		return result, err
	}
	after, err := f.Stat()
	if err != nil {
		return result, err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || n != after.Size() {
		return result, ErrRecoveryCandidateChanged
	}
	// Re-open through the rooted boundary to also detect replacement of the name.
	current, err := payload.Open(root, path)
	if err != nil {
		return result, err
	}
	defer current.Close()
	id, err := storage.FileIdentity(current)
	if err != nil || id != result.Identity {
		return result, ErrRecoveryCandidateChanged
	}
	result.Size = n
	result.Modified = after.ModTime().UnixNano()
	result.Digest = hex.EncodeToString(h.Sum(nil))
	return result, nil
}
func matchesRecovery(ctx context.Context, root *os.Root, path string, want recoveryFingerprint, observe func(int64)) error {
	got, err := fingerprintRecovery(ctx, root, path, observe)
	if err != nil {
		return err
	}
	if got != want {
		return ErrRecoveryCandidateChanged
	}
	return nil
}

func (uc *TorrentUseCase) recoveryLayout(hash string) (*models.Torrent, *repository.StorageMapping, *metainfo.MetaInfo, error) {
	t, err := uc.repo.GetTorrent(hash)
	if err != nil {
		return nil, nil, nil, err
	}
	if t == nil {
		return nil, nil, nil, ErrTorrentNotFound
	}
	if t.DeletionMode != "" || uc.transferProtected(hash) {
		return nil, nil, nil, ErrStorageConflict
	}
	m, err := uc.repo.StorageMapping(hash)
	if err != nil {
		return nil, nil, nil, err
	}
	if !m.Ready() {
		return nil, nil, nil, ErrStorageConflict
	}
	mi, err := metainfo.LoadFromFile(filepath.Join(uc.engine.ConfigDir(), "metainfo", hash+".torrent"))
	if err != nil {
		return nil, nil, nil, err
	}
	spec, specErr := torrent.TorrentSpecFromMetaInfoErr(mi)
	if specErr != nil {
		return nil, nil, nil, specErr
	}
	matches := spec.InfoHash.HexString() == hash
	if spec.InfoHashV2.Ok {
		v2 := spec.InfoHashV2.Value
		matches = matches || v2.ToShort().HexString() == hash
	}
	if !matches {
		return nil, nil, nil, ErrStorageConflict
	}
	info, err := uc.metadataStorageInfo(t)
	if err != nil || info == nil {
		if err == nil {
			err = ErrMetadataPending
		}
		return nil, nil, nil, err
	}
	return t, m, mi, nil
}

// StartRecovery admits only one heavy recovery job globally. It is engine-owned,
// so HTTP disconnect/shutdown never closes a resource still owned by a syscall.
func (uc *TorrentUseCase) StartRecovery(ctx context.Context, hash, id string, version int64, action, choice string) error {
	if id == "" || version < 1 {
		return ErrStorageConflict
	}
	release, err := uc.lockRecord(hash, ctx)
	if err != nil {
		return err
	}
	rec, err := uc.recoveryExpected(hash, id, version)
	if err != nil {
		release()
		return err
	}
	saved, readErr := uc.repo.GetTorrent(hash)
	if readErr != nil {
		release()
		return readErr
	}
	if saved == nil || saved.DeletionMode != "" || uc.transferProtected(hash) {
		release()
		return ErrStorageConflict
	}
	mapping, readErr := uc.repo.StorageMapping(hash)
	if readErr != nil {
		release()
		return readErr
	}
	if mapping == nil || ((action == "analyze_legacy" || action == "apply_legacy") && (mapping.State != "ready" || mapping.Manifest != "")) {
		release()
		return ErrStorageConflict
	}
	release()
	if action != "analyze_legacy" && action != "apply_legacy" && action != "resume_verify" {
		return ErrStorageConflict
	}
	if action == "apply_legacy" && choice != "final" && choice != "part" {
		return ErrStorageConflict
	}
	if action == "analyze_legacy" && rec.Reason != models.StorageLegacyConflict {
		return ErrStorageConflict
	}
	if action == "resume_verify" && rec.Reason != models.StorageDeletionCancelled && rec.Reason != models.StorageAttachmentFailed && rec.Reason != models.StorageRecoveryComplete {
		return ErrStorageConflict
	}
	ticket, err := uc.coordinator.tryAcquire("storage/recovery")
	if err != nil {
		return ErrStorageConflict
	}
	mutation, err := uc.coordinator.tryAcquire("storage/mutation")
	if err != nil {
		ticket.release()
		return ErrStorageConflict
	}
	if !uc.engine.RunStorageTask(func(workerCtx context.Context) {
		defer ticket.release()
		defer mutation.release()
		workerCtx = context.WithValue(workerCtx, storageMutationHeld{}, true)
		var opErr error
		switch action {
		case "analyze_legacy":
			opErr = uc.analyzeLegacy(workerCtx, hash, id, version)
		case "apply_legacy":
			opErr = uc.applyLegacy(workerCtx, hash, id, version, choice)
		case "resume_verify":
			opErr = uc.resumeVerifiedRecovery(workerCtx, hash, id, version)
		}
		if opErr != nil {
			uc.saveRecoveryFailure(hash, id, version, opErr)
		}
	}) {
		ticket.release()
		mutation.release()
		return engine.ErrTaskLimit
	}
	return nil
}

func (uc *TorrentUseCase) analyzeLegacy(ctx context.Context, hash, id string, version int64) error {
	release, err := uc.lockRecord(hash, ctx)
	if err != nil {
		return err
	}
	rec, err := uc.recoveryExpected(hash, id, version)
	if err != nil {
		release()
		return err
	}
	if rec.Reason != models.StorageLegacyConflict || rec.Stage == "preserved" || rec.Stage == "published" {
		release()
		return ErrStorageConflict
	}
	_, m, mi, err := uc.recoveryLayout(hash)
	if err != nil {
		release()
		return err
	}
	old, err := decodeLegacyJournal(rec)
	if err != nil {
		release()
		return err
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		release()
		return err
	}
	paths, err := relativeStoragePaths(m, &info)
	if err != nil {
		release()
		return err
	}
	root, err := os.OpenRoot(uc.engine.DownloadDir())
	if err != nil {
		release()
		return err
	}
	defer root.Close()
	if rec.Stage == "prepared" && old.Backup != "" {
		if _, err := root.Lstat(old.Backup); err == nil {
			release()
			return ErrStorageConflict
		} else if !errors.Is(err, os.ErrNotExist) {
			release()
			return err
		}
	}
	claim, err := uc.reserveOwnership(ctx, root, hash, []string{m.Root})
	if err != nil {
		release()
		return err
	}
	defer claim()
	if err := uc.engine.Delete(hash); err != nil {
		release()
		return err
	}
	// Pause is durable; recovery completion never automatically starts downloads.
	i, err := uc.currentIntent(hash)
	if err == nil {
		_, err = uc.repo.CommitRunIntent(i.Version, models.RunPaused)
	}
	if err != nil {
		release()
		return err
	}
	j := legacyRecoveryJournal{Alternatives: old.Alternatives}
	index := -1
	for idx, path := range paths {
		candidate := path + ".part"
		isMetadata := false
		for _, other := range paths {
			if ownershipOverlap(candidate, other) {
				isMetadata = true
				break
			}
		}
		if isMetadata {
			continue
		}
		_, a := root.Lstat(filepath.Join(m.Root, path))
		_, b := root.Lstat(filepath.Join(m.Root, candidate))
		if a == nil && b == nil {
			j.MetadataPath = path
			index = idx
			break
		}
		if a != nil && !errors.Is(a, os.ErrNotExist) {
			release()
			return a
		}
		if b != nil && !errors.Is(b, os.ErrNotExist) {
			release()
			return b
		}
	}
	if index < 0 {
		release()
		return fmt.Errorf("no legacy conflict remains; retry verification")
	}
	if err := uc.repo.NextRecoveryVersion(rec); err != nil {
		release()
		return err
	}
	version = rec.Version
	rec.Error = ""
	rec.FailureCode = ""
	rec.Reason = models.StorageLegacyConflict
	if err := uc.saveLegacyJournal(rec, &j, "analyzing"); err != nil {
		release()
		return err
	}
	release()
	stop := uc.startStorageProgress(hash, id, version, models.StorageLegacyConflict)
	defer stop()
	uc.storageStage(hash, "analysis", nil, int64ptr(2))
	observe := func(n int64) { uc.storageBytes(hash, n) }
	path := j.MetadataPath
	uc.storageFile(hash, path, nil)
	j.Final, err = fingerprintRecovery(ctx, root, filepath.Join(m.Root, path), observe)
	if err != nil {
		return err
	}
	uc.storageFileDone(hash)
	uc.storageFile(hash, path+".part", nil)
	j.Part, err = fingerprintRecovery(ctx, root, filepath.Join(m.Root, path+".part"), observe)
	if err != nil {
		return err
	}
	uc.storageFileDone(hash)
	// The verifier checks the whole torrent, preserving v1 shared-boundary and
	// v2/hybrid semantics. Missing neighbors make evidence inconclusive, not valid.
	uc.storageStage(hash, "verification", nil, nil)
	final, err := engine.VerifyReadOnlyLayout(ctx, uc.engine.DownloadDir(), m.Layout(), mi, nil, uc.storageIORead(hash))
	if err != nil {
		return err
	}
	part, err := engine.VerifyReadOnlyLayout(ctx, uc.engine.DownloadDir(), m.Layout(), mi, map[string]string{path: path + ".part"}, uc.storageIORead(hash))
	if err != nil {
		return err
	}
	if index >= len(final.Files) || index >= len(part.Files) {
		return ErrStorageConflict
	}
	a, b := final.Files[index], part.Files[index]
	analysis := &models.LegacyAnalysis{Path: path, Equal: j.Final.Size == j.Part.Size && j.Final.Digest == j.Part.Digest, Final: models.LegacyCandidateEvidence{Size: j.Final.Size, ValidPieces: a.ValidPieces, TotalPieces: a.TotalPieces}, Part: models.LegacyCandidateEvidence{Size: j.Part.Size, ValidPieces: b.ValidPieces, TotalPieces: b.TotalPieces}, Limitations: []string{"Whole-file alternatives only; incomplete shared boundary pieces cannot prove a candidate. Analysis does not grant runtime readiness."}}
	if analysis.Equal {
		analysis.Recommended = "final"
	} else if a.ValidPieces == a.TotalPieces && a.TotalPieces > 0 && b.ValidPieces < b.TotalPieces {
		analysis.Recommended = "final"
	} else if b.ValidPieces == b.TotalPieces && b.TotalPieces > 0 && a.ValidPieces < a.TotalPieces {
		analysis.Recommended = "part"
	}
	j.Analysis = analysis
	// Candidates must still match the analyzed descriptors after both hash passes.
	if err := matchesRecovery(ctx, root, filepath.Join(m.Root, path), j.Final, observe); err != nil {
		return err
	}
	if err := matchesRecovery(ctx, root, filepath.Join(m.Root, path+".part"), j.Part, observe); err != nil {
		return err
	}
	release, err = uc.lockRecord(hash, ctx)
	if err != nil {
		return err
	}
	defer release()
	if _, err := uc.recoveryExpected(hash, id, version); err != nil {
		return err
	}
	return uc.saveLegacyJournal(rec, &j, "analyzed")
}

func (uc *TorrentUseCase) applyLegacy(ctx context.Context, hash, id string, version int64, choice string) error {
	release, err := uc.lockRecord(hash, ctx)
	if err != nil {
		return err
	}
	rec, err := uc.recoveryExpected(hash, id, version)
	if err != nil {
		release()
		return err
	}
	j, err := decodeLegacyJournal(rec)
	if err != nil {
		release()
		return err
	}
	if j.Analysis == nil || j.MetadataPath == "" {
		release()
		return ErrStorageConflict
	}
	if rec.Stage == "verified" {
		release()
		return nil
	}
	if rec.Stage != "analyzed" && rec.Stage != "prepared" && rec.Stage != "preserved" && rec.Stage != "published" {
		release()
		return ErrStorageConflict
	}
	if choice != "final" && choice != "part" {
		release()
		return ErrStorageConflict
	}
	if j.Choice != "" && j.Choice != choice {
		release()
		return ErrStorageConflict
	}
	_, m, mi, err := uc.recoveryLayout(hash)
	if err != nil {
		release()
		return err
	}
	if err := uc.revokeResumeMutation(ctx, hash); err != nil {
		release()
		return err
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		release()
		return err
	}
	metadataPaths, err := relativeStoragePaths(m, &info)
	if err != nil {
		release()
		return err
	}
	validPath := false
	for _, p := range metadataPaths {
		if p == j.MetadataPath {
			validPath = true
		}
		if ownershipOverlap(j.MetadataPath+".part", p) {
			release()
			return ErrStorageConflict
		}
	}
	if !validPath || len(id) != 32 || strings.ContainsAny(id, "/\\.") {
		release()
		return ErrStorageConflict
	}
	expectedBackup := filepath.Join(".tsh-recovery", id, strconv.FormatInt(version, 10), "alternative")
	if j.Backup != "" && j.Backup != expectedBackup {
		release()
		return ErrStorageConflict
	}
	root, err := os.OpenRoot(uc.engine.DownloadDir())
	if err != nil {
		release()
		return err
	}
	defer root.Close()
	claim, err := uc.reserveOwnership(ctx, root, hash, []string{m.Root})
	if err != nil {
		release()
		return err
	}
	defer claim()
	if err := uc.engine.Delete(hash); err != nil {
		release()
		return err
	}
	finalPath := filepath.Join(m.Root, j.MetadataPath)
	partPath := finalPath + ".part"
	alternative, selected := partPath, finalPath
	altStamp, selectedStamp := j.Part, j.Final
	if choice == "part" {
		alternative, selected = finalPath, partPath
		altStamp, selectedStamp = j.Final, j.Part
	}
	if j.Backup == "" {
		if len(id) != 32 || strings.ContainsAny(id, "/\\.") {
			release()
			return ErrStorageConflict
		}
		j.Backup = filepath.Join(".tsh-recovery", id, strconv.FormatInt(version, 10), "alternative")
		j.Choice = choice
	}
	if rec.Stage == "analyzed" {
		// Freeze journal choice before the first namespace mutation.
		if err := uc.saveLegacyJournal(rec, &j, "prepared"); err != nil {
			release()
			return err
		}
	}
	release()
	stop := uc.startStorageProgress(hash, id, version, models.StorageLegacyConflict)
	defer stop()
	observe := func(n int64) { uc.storageBytes(hash, n) }
	uc.storageStage(hash, "preflight", nil, nil)
	if err := uc.recoveryBoundary("prepared"); err != nil {
		return err
	}
	move := uc.recoveryMove
	if move == nil {
		move = storage.MoveRootNoReplace
	}
	uncheckedMove := move
	move = func(root *os.Root, source, target string) error {
		if err := uc.guardPayloadIO(source); err != nil {
			return err
		}
		if err := uc.guardPayloadIO(target); err != nil {
			return err
		}
		return uncheckedMove(root, source, target)
	}
	if _, err := root.Lstat(j.Backup); errors.Is(err, os.ErrNotExist) {
		if err := matchesRecovery(ctx, root, alternative, altStamp, observe); err != nil {
			return err
		}
		if err := matchesRecovery(ctx, root, selected, selectedStamp, observe); err != nil {
			return err
		}
		if err := uc.engine.CheckMigrationSpace(uc.engine.DownloadDir(), 0); err != nil {
			return err
		}
		if err := payload.Validate(root, j.Backup); err != nil {
			return err
		}
		if err := uc.guardPayloadIO(j.Backup); err != nil {
			return err
		}
		if err := root.MkdirAll(filepath.Dir(j.Backup), 0700); err != nil {
			return err
		}
		uc.storageStage(hash, "preserve_alternative", nil, nil)
		if err := move(root, alternative, j.Backup); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := matchesRecovery(ctx, root, j.Backup, altStamp, observe); err != nil {
		if errors.Is(err, ErrRecoveryCandidateChanged) {
			return fmt.Errorf("%w: alternative destination is not journal-owned", os.ErrExist)
		}
		return err
	}
	if err := storage.SyncRootPublication(root, j.Backup); err != nil {
		return err
	}
	if err := uc.recoveryBoundary("alternative_moved"); err != nil {
		return err
	}
	// Once published, the alternative's former name can be the selected payload.
	if rec.Stage != "published" {
		if err := uc.saveLegacyJournal(rec, &j, "preserved"); err != nil {
			return err
		}
	}
	uc.storageStage(hash, "publication", nil, nil)
	if choice == "part" {
		if _, err := root.Lstat(partPath); err == nil {
			if err := matchesRecovery(ctx, root, partPath, selectedStamp, observe); err != nil {
				return err
			}
			if err := uc.engine.CheckMigrationSpace(uc.engine.DownloadDir(), 0); err != nil {
				return err
			}
			if err := move(root, partPath, finalPath); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := matchesRecovery(ctx, root, finalPath, selectedStamp, observe); err != nil {
		return err
	}
	if err := storage.SyncRootPublication(root, finalPath); err != nil {
		return err
	}
	if err := uc.recoveryBoundary("selected_published"); err != nil {
		return err
	}
	if err := uc.saveLegacyJournal(rec, &j, "published"); err != nil {
		return err
	}
	uc.storageStage(hash, "verification", nil, nil)
	if _, err := engine.VerifyReadOnlyLayout(ctx, uc.engine.DownloadDir(), m.Layout(), mi, nil, uc.storageIORead(hash)); err != nil {
		return err
	}
	if err := uc.recoveryBoundary("verified"); err != nil {
		return err
	}
	release, err = uc.lockRecord(hash, ctx)
	if err != nil {
		return err
	}
	defer release()
	if _, err := uc.recoveryExpected(hash, id, version); err != nil {
		return err
	}
	already := false
	for _, a := range j.Alternatives {
		if a.Path == j.Backup {
			already = true
		}
	}
	if !already {
		j.Alternatives = append(j.Alternatives, preservedAlternative{Path: j.Backup, Size: altStamp.Size})
	}
	rec.Reason = models.StorageRecoveryComplete
	rec.Error = ""
	rec.FailureCode = ""
	return uc.saveLegacyJournal(rec, &j, "verified")
}

func (uc *TorrentUseCase) resumeVerifiedRecovery(ctx context.Context, hash, id string, version int64) error {
	release, err := uc.lockRecord(hash, ctx)
	if err != nil {
		return err
	}
	rec, err := uc.recoveryExpected(hash, id, version)
	if err != nil {
		release()
		return err
	}
	if recoveryBlocks(rec) && rec.Reason != models.StorageDeletionCancelled && rec.Stage != "verifying" {
		release()
		return ErrStorageConflict
	}
	advanced := false
	if rec.Reason == models.StorageDeletionCancelled {
		mapping, err := uc.repo.StorageMapping(hash)
		if err != nil {
			release()
			return err
		}
		if mapping != nil && (mapping.State != "ready" || mapping.Manifest != "") {
			saved, err := uc.repo.GetTorrent(hash)
			if err != nil {
				release()
				return err
			}
			if saved == nil || saved.DeletionMode != "" || uc.transferProtected(hash) {
				release()
				return ErrStorageConflict
			}
			if err := uc.repo.NextRecoveryVersion(rec); err != nil {
				release()
				return err
			}
			version = rec.Version
			advanced = true
			rec.Stage = "verifying"
			rec.Error = ""
			rec.FailureCode = ""
			if err := uc.repo.SaveRecovery(rec); err != nil {
				release()
				return err
			}
			release()
			migrationCtx := context.WithValue(ctx, cancelledMigrationKey{}, cancelledMigrationAuthorization{recordID: id, version: version})
			if err := uc.migrateStorage(migrationCtx, saved); err != nil {
				return err
			}
			release, err = uc.lockRecord(hash, ctx)
			if err != nil {
				return err
			}
			rec, err = uc.recoveryExpected(hash, id, version)
			if err != nil {
				release()
				return err
			}
		}
	}
	_, m, mi, err := uc.recoveryLayout(hash)
	if err != nil {
		release()
		return err
	}
	if err := uc.engine.Delete(hash); err != nil {
		release()
		return err
	}
	root, err := os.OpenRoot(uc.engine.DownloadDir())
	if err != nil {
		release()
		return err
	}
	defer root.Close()
	claim, err := uc.reserveOwnership(ctx, root, hash, []string{m.Root})
	if err != nil {
		release()
		return err
	}
	defer claim()
	if !advanced {
		if err := uc.repo.NextRecoveryVersion(rec); err != nil {
			release()
			return err
		}
	}
	version = rec.Version
	rec.Stage = "verifying"
	rec.Error = ""
	rec.FailureCode = ""
	if err := uc.repo.SaveRecovery(rec); err != nil {
		release()
		return err
	}
	release()
	stop := uc.startStorageProgress(hash, id, version, rec.Reason)
	defer stop()
	uc.storageStage(hash, "verification", nil, nil)
	if _, err := engine.VerifyReadOnlyLayout(ctx, uc.engine.DownloadDir(), m.Layout(), mi, nil, uc.storageIORead(hash)); err != nil {
		return err
	}
	release, err = uc.lockRecord(hash, ctx)
	if err != nil {
		return err
	}
	defer release()
	if _, err := uc.recoveryExpected(hash, id, version); err != nil {
		return err
	}
	rec.Reason = models.StorageRecoveryComplete
	rec.Stage = "verified"
	rec.Error = ""
	if err := uc.repo.SaveRecovery(rec); err != nil {
		return err
	}
	for {
		ready, err := uc.inspectInventory(ctx, hash, false)
		if err != nil {
			return err
		}
		if ready {
			break
		}
		state, err := uc.repo.Inventory(m.StorageID)
		if err != nil {
			return err
		}
		if state == nil || state.Status == "blocked" {
			return ErrStorageConflict
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	return uc.resumeLocked(hash)
}

type cancelledMigrationKey struct{}
type cancelledMigrationAuthorization struct {
	recordID string
	version  int64
}

func (uc *TorrentUseCase) saveRecoveryFailure(hash, id string, version int64, cause error) {
	if errors.Is(cause, ErrStorageConflict) || errors.Is(cause, repository.ErrStateConflict) {
		return
	}
	rec, err := uc.repo.Recovery(hash)
	if err != nil || rec == nil || rec.RecordID != id || (rec.Version != version && rec.Version != version+1) {
		return
	}
	rec.FailureCode = "io_error"
	rec.Error = "Recovery I/O failed; candidates preserved; retry the current operation"
	switch {
	case errors.Is(cause, errCoordinatorBusy):
		rec.FailureCode = "operation_busy"
		rec.Error = "Another migration is active; retry after it finishes"
	case errors.Is(cause, ErrRecoveryCandidateChanged):
		rec.FailureCode = "candidate_changed"
		rec.Error = "Candidate changed; analyze again before applying"
	case errors.Is(cause, engine.ErrMigrationDiskFull) || errors.Is(cause, syscall.ENOSPC):
		rec.FailureCode = "disk_full"
		rec.Error = "Insufficient free space; candidates preserved"
	case errors.Is(cause, storage.ErrPublicationFlush):
		rec.FailureCode = "flush_failed"
		rec.Error = "Flush failed; publication may already have completed; retry the journal without deleting retained copies"
	case errors.Is(cause, storage.ErrUnsafeFile) || errors.Is(cause, payload.ErrNotRegular) || errors.Is(cause, payload.ErrUnsafePath):
		rec.FailureCode = "unsafe_candidate"
		rec.Error = "Unsafe candidate rejected; no automatic cleanup"
	case errors.Is(cause, os.ErrExist):
		rec.FailureCode = "target_exists"
		rec.Error = "Recovery target already exists; no replacement performed"
	case errors.Is(cause, os.ErrNotExist):
		rec.FailureCode = "candidate_missing"
		rec.Error = "Candidate missing; inspect retained copies before retry"
	case errors.Is(cause, os.ErrInvalid) || errors.Is(cause, syscall.EXDEV) || errors.Is(cause, syscall.ENOSYS) || errors.Is(cause, syscall.ENOTSUP):
		rec.FailureCode = "operation_unsupported"
		rec.Error = "Filesystem does not support safe publication"
	}
	_ = uc.repo.SaveRecovery(rec)
}

func relativeStoragePaths(m *repository.StorageMapping, info *metainfo.Info) ([]string, error) {
	paths, err := storagePaths(info)
	if err != nil {
		return nil, err
	}
	for i, p := range paths {
		paths[i], err = m.Layout().Relative(p)
		if err != nil {
			return nil, err
		}
	}
	return paths, nil
}
