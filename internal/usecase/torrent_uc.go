package usecase

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

var ErrTorrentNotFound = repository.ErrTorrentNotFound
var ErrFileNotFound = repository.ErrFileNotFound
var ErrInvalidPriority = repository.ErrInvalidPriority
var ErrMetadataPending = repository.ErrMetadataPending
var ErrDownloadNotReady = errors.New("download is not ready")
var ErrStorageConflict = errors.New("storage operation conflicts with torrent state or ownership")

type TorrentNotFoundError struct {
	Hash string
}

func (e TorrentNotFoundError) Error() string {
	return fmt.Sprintf("torrent not found: %s", e.Hash)
}

func (e TorrentNotFoundError) Is(target error) bool {
	return target == ErrTorrentNotFound
}

type TorrentUseCase struct {
	volumeMu             sync.RWMutex
	volumes              []payload.VolumeRegistration
	inventorySession     string
	inventoryInitError   error
	inventoryWake        chan struct{}
	engine               *engine.Engine
	repo                 *repository.TorrentRepo
	coordinator          *applicationCoordinator
	policyMu             sync.Mutex // policy/lease data only, never SQLite/engine/I/O
	storageInitMu        sync.Mutex // DB-only initialization, never waits for command keys
	cleanupMu            sync.Mutex // cleanup uses TryLock; commands never wait for a cleanup pass
	maxTemporary         int
	transfers            map[string]int
	protectedUntil       map[string]time.Time
	transferRecords      map[string]string
	storageWorkerStarted bool
	// Optional rooted filesystem operation for deterministic failure injection.
	payloadRemove           func(*os.Root, string) error
	beforeDetach            func(string)
	inactiveVerify          func(context.Context, string) error
	migrationCopy           func(context.Context, *os.Root, *migrationEntry) error
	ownershipMu             sync.Mutex // short admission/SQL publication only; never filesystem I/O
	ownershipClaims         map[*ownershipClaim]bool
	beforeOwnershipValidate func() // deterministic catalog mutation barrier
	storageLiveMu           sync.Mutex
	storageLive             map[string]*storageObservation
	recoveryMove            func(*os.Root, string, string) error
	recoveryStep            func(string) error
	storageWake             chan struct{}
	layoutWake              chan string
	locationWake            chan struct{}
	locationWorkMu          sync.Mutex
	locationWork            *activeLocationWork
	locationMove            func(*os.Root, string, string) error
	storageByteHook         func(string, int64)
	storageStageHook        func(string, models.StorageProgress, time.Duration)
	migrationFailures       map[string]migrationFailure
}

func (uc *TorrentUseCase) NextSnapshotVersion() string { return uc.engine.NextSnapshotVersion() }

type StreamOptions = engine.StreamOptions

type TorrentMetadata struct {
	PrivateSafe bool
	Title       string
	Data        string
	Poster      string
	Category    string
	FolderName  string
}

type DownloadedDiskFile struct {
	Index int
	Path  string
	Size  int64
	open  func() (*os.File, error)
}

// Open revalidates ownership and opens through os.Root, never an absolute path.
func (f DownloadedDiskFile) Open() (*os.File, error) {
	if f.open == nil {
		return nil, ErrStorageConflict
	}
	return f.open()
}

func NewTorrentUseCase(e *engine.Engine, r *repository.TorrentRepo) *TorrentUseCase {
	e.SetDownloadBlockPersistence(r.SaveDownloadBlock)
	e.SetSafetyPersistence(r.PutRuntimeSafetyBlock)
	e.SetVerificationPersistence(func(f models.RuntimeFence, uncertain bool, reason string) error {
		if uncertain {
			return r.PutActivationBlock(f, models.BlockVerification, reason)
		}
		return r.ClearActivationBlock(f, models.BlockVerification)
	})
	uc := &TorrentUseCase{
		engine: e, repo: r, maxTemporary: 3, transfers: make(map[string]int),
		protectedUntil: make(map[string]time.Time), transferRecords: make(map[string]string),
		coordinator: newApplicationCoordinator(e.Context()), storageWake: make(chan struct{}, 1),
		layoutWake:    make(chan string, 64),
		locationWake:  make(chan struct{}, 1),
		inventoryWake: make(chan struct{}, 1),
	}
	uc.initializeInventory()
	e.SetStorageIOGuard(uc.guardPayloadIO)
	e.SetStorageLayoutResolver(func(hash string, info *metainfo.Info) (payload.Layout, error) {
		m, err := r.StorageMapping(hash)
		if err != nil {
			return payload.Layout{}, err
		}
		if m != nil && m.State == "reserved" && m.Version == payload.NamedLayoutVersion {
			if info != nil {
				select {
				case uc.layoutWake <- hash:
				default:
				}
			}
			return payload.Layout{}, storage.ErrStoragePending
		}
		if !m.Ready() {
			return payload.Layout{}, ErrStorageConflict
		}
		if err := uc.inventoryAdmitted(m); err != nil {
			return payload.Layout{}, storage.ErrStoragePending
		}
		return m.Layout(), nil
	}, uc.claimStorageAlias)
	e.StartTask(uc.runLayoutReservations)
	e.StartTask(uc.runLocationOperations)
	e.StartTask(uc.runInventory)
	e.StartTask(func(ctx context.Context) { <-ctx.Done(); _ = uc.coordinator.close(context.Background()) })
	return uc
}

func (uc *TorrentUseCase) AddMagnet(magnet string) (*models.Torrent, error) {
	return uc.AddMagnetWithMetadata(magnet, TorrentMetadata{})
}

func (uc *TorrentUseCase) AddMagnetWithMetadata(magnet string, metadata TorrentMetadata) (*models.Torrent, error) {
	return uc.AddMagnetWithPolicy(magnet, metadata, false)
}

func (uc *TorrentUseCase) AddMagnetWithPolicy(magnet string, metadata TorrentMetadata, temporary bool) (*models.Torrent, error) {
	if metadata.PrivateSafe {
		u, err := url.Parse(magnet)
		if err != nil {
			return nil, err
		}
		q := u.Query()
		q.Set("x.hub-private", "1")
		u.RawQuery = q.Encode()
		magnet = u.String()
	}

	logging.Infof("usecase add magnet %s", logging.SafeMagnetSummary(magnet))
	parsed, err := torrent.TorrentSpecFromMagnetUri(magnet)
	if err != nil {
		return nil, err
	}
	release, err := uc.lockSpec(parsed)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := uc.checkSpecAddAllowed(parsed, metadata.FolderName); err != nil {
		return nil, err
	}
	// Mapping/alias reservation is now durable and insert-only. Reacquire the
	// complete resolved root-key set before engine/intent effects: asynchronous
	// metadata may have introduced an alias while the initial keys were resolved.
	release()
	release, err = uc.lockSpec(parsed)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := uc.checkSpecAddAllowed(parsed, metadata.FolderName); err != nil {
		return nil, err
	}
	t, err := uc.engine.AddStagedMagnet(magnet)
	if err != nil {
		logging.Warnf("usecase add magnet failed %s: %v", logging.SafeMagnetSummary(magnet), err)
		return nil, err
	}

	return uc.finishAddLocked(t, metadata, temporary)
}

func (uc *TorrentUseCase) AddTorrentFile(r io.Reader) (*models.Torrent, error) {
	return uc.AddTorrentFileWithPolicy(r, TorrentMetadata{}, false)
}

func (uc *TorrentUseCase) AddTorrentFileWithPolicy(r io.Reader, metadata TorrentMetadata, temporary bool) (*models.Torrent, error) {
	logging.Infof("usecase add .torrent file")
	mi, err := engine.LoadMetainfo(r)
	if err != nil {
		return nil, err
	}
	spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		return nil, err
	}
	spec.PrivateSafe = metadata.PrivateSafe
	var encoded bytes.Buffer
	if err := mi.Write(&encoded); err != nil {
		return nil, err
	}
	// Input belongs to the caller and may block indefinitely. No canonical
	// identity/namespace has been reserved yet; validate duplicates/workflows only
	// after parsing, under coordination, before touching the engine.
	release, err := uc.lockSpec(spec)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := uc.checkSpecAddAllowed(spec, metadata.FolderName); err != nil {
		return nil, err
	}
	release()
	release, err = uc.lockSpec(spec)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := uc.checkSpecAddAllowed(spec, metadata.FolderName); err != nil {
		return nil, err
	}
	t, err := uc.engine.AddStagedTorrentFile(&encoded, metadata.PrivateSafe)
	if err != nil {
		logging.Warnf("usecase add .torrent file failed: %v", err)
		return nil, err
	}

	return uc.finishAddLocked(t, metadata, temporary)
}

// Caller owns hash/root keys; reject re-add before engine mutation/admission.
func (uc *TorrentUseCase) checkSpecAddAllowed(spec *torrent.TorrentSpec, names ...string) error {
	if err := uc.initializeStorageLocked(); err != nil {
		return err
	}
	if err := uc.checkAddAllowed(spec.InfoHash.HexString()); err != nil {
		return err
	}
	if spec.InfoHashV2.Ok {
		v2 := spec.InfoHashV2.Value
		if err := uc.checkAddAllowed(v2.ToShort().HexString()); err != nil {
			return err
		}
	}
	hash := spec.InfoHash.HexString()
	if spec.InfoHash == (metainfo.Hash{}) && spec.InfoHashV2.Ok {
		v2 := spec.InfoHashV2.Value
		hash = v2.ToShort().HexString()
	}
	if spec.PrivateSafe {
		hashes := []string{hash}
		if spec.InfoHashV2.Ok {
			hashes = append(hashes, spec.InfoHashV2.Value.ToShort().HexString())
		}
		for _, candidate := range hashes {
			owner, err := uc.repo.StorageOwner(candidate)
			if err != nil {
				return err
			}
			if owner == "" {
				continue
			}
			existing, err := uc.repo.GetTorrent(owner)
			if err != nil {
				return err
			}
			if existing != nil {
				old, err := torrent.TorrentSpecFromMagnetUri(existing.SourceURI)
				if err != nil || !old.PrivateSafe {
					return fmt.Errorf("private-safe requires a new record: %w", ErrStorageConflict)
				}
			}
		}
	}
	var alias string
	if spec.InfoHashV2.Ok {
		v2 := spec.InfoHashV2.Value
		alias = v2.ToShort().HexString()
	}
	if err := uc.reserveStorageLocked(hash, alias, names...); err != nil {
		return err
	}
	if len(spec.InfoBytes) > 0 {
		mi := &metainfo.MetaInfo{InfoBytes: spec.InfoBytes}
		info, err := mi.UnmarshalInfo()
		if err != nil {
			return err
		}
		return uc.prepareNamedStorage(uc.engine.Context(), hash, &info)
	}
	return nil
}

func (uc *TorrentUseCase) checkAddAllowed(hash string) error {
	t, err := uc.repo.GetTorrent(hash)
	if err != nil || t == nil {
		return err
	}
	if t.DownloadBlock != "" {
		return engine.DownloadBlockedError{Reason: t.DownloadBlock}
	}
	if current := uc.engine.GetTorrent(hash); current != nil {
		if current.DownloadBlock != "" {
			return engine.DownloadBlockedError{Reason: current.DownloadBlock}
		}
	} else {
		// A duplicate add is not a Resume. DB-only records must use the staged
		// restore path, otherwise default priorities can bypass saved user intent.
		return ErrStorageConflict
	}
	_, pending, err := uc.repo.CleanupPaths(hash)
	if err != nil {
		return err
	}
	if pending {
		return ErrStorageConflict
	}
	if err := uc.checkStorageAvailable(hash); err != nil {
		return err
	}
	return nil
}

func (uc *TorrentUseCase) GetAllTorrents() ([]*models.Torrent, error) {
	started := time.Now().UnixNano()
	merged, err := uc.readAllConsistently()
	if err != nil {
		return nil, err
	}

	for _, t := range merged {
		t.SnapshotAt = started
	}
	uc.decorateStorageList(merged)
	return merged, nil
}

func (uc *TorrentUseCase) BTHealth() *models.BTHealth {
	return uc.engine.BTHealth()
}

func (uc *TorrentUseCase) GetTorrent(hash string) (*models.Torrent, error) {
	started := time.Now().UnixNano()
	readDB := func() ([]*models.Torrent, error) {
		v, err := uc.repo.GetTorrent(hash)
		if err != nil {
			return nil, err
		}
		if v == nil {
			return []*models.Torrent{}, nil
		}
		return []*models.Torrent{v}, nil
	}
	readRuntime := func() []*models.Torrent {
		v := uc.engine.GetTorrentCached(hash)
		if v == nil {
			return nil
		}
		return []*models.Torrent{v}
	}
	items, err := readConsistentTorrents(func() (int64, error) { return uc.repo.ReadVersion(hash) }, readDB, readRuntime, func() func() bool {
		stamp := uc.engine.CaptureReadStamp(hash)
		return func() bool { return uc.engine.ValidateReadStamp(stamp) }
	})
	if err != nil {
		return nil, err
	}
	var t *models.Torrent
	if len(items) > 0 {
		t = items[0]
	}
	if t != nil {
		uc.decorateStorageList([]*models.Torrent{t})
		t.SnapshotAt = started
	}
	return t, nil
}

// GetTorrentPieces returns a string representing the status of all pieces in a torrent.
// "2" = downloaded, "1" = partial/downloading, "0" = missing.
// Returns an empty string if torrent or metadata is not available.
func (uc *TorrentUseCase) GetTorrentPieces(hash string) (string, error) {
	if model := uc.engine.GetTorrentCached(hash); model != nil && model.ProgressSource == "checking" {
		return "", nil
	}
	result, err := uc.engine.PieceStatus(hash)
	if errors.Is(err, engine.ErrTorrentNotFound) {
		return "", TorrentNotFoundError{Hash: hash}
	}
	return result, err
}

func (uc *TorrentUseCase) UpdateMetadata(hash string, metadata TorrentMetadata) (*models.Torrent, error) {
	release, err := uc.lockRecord(hash)
	if err != nil {
		return nil, err
	}
	defer release()
	t, err := uc.repo.GetTorrent(hash)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, TorrentNotFoundError{Hash: hash}
	}
	i, err := uc.currentIntent(hash)
	if err != nil {
		return nil, err
	}
	_, err = uc.runIntentCommand(i.Version, false, false, func() (models.IntentVersion, error) {
		return uc.repo.UpdateUserMetadata(i.Version, models.UserMetadata{Title: metadata.Title, Data: metadata.Data, Poster: metadata.Poster, Category: metadata.Category})
	}, nil)
	if err != nil {
		return nil, err
	}
	return uc.GetTorrent(hash)
}

func (uc *TorrentUseCase) Pause(hash string, contexts ...context.Context) error {
	release, err := uc.lockRecord(hash, contexts...)
	if err != nil {
		return err
	}
	defer release()
	logging.Infof("usecase pause hash=%s", hash)
	t, err := uc.repo.GetTorrent(hash)
	if err != nil {
		logging.Warnf("pause failed to load torrent hash=%s: %v", hash, err)
		return err
	}
	if t == nil {
		logging.Debugf("pause requested for missing torrent hash=%s", hash)
		return TorrentNotFoundError{Hash: hash}
	}

	i, err := uc.currentIntent(hash)
	if err != nil {
		return err
	}
	expected := uc.engine.GetRawTorrent(hash)
	if err := uc.engine.Pause(hash); err != nil && !errors.Is(err, engine.ErrTorrentNotFound) {
		logging.Warnf("engine pause failed hash=%s: %v", hash, err)
		return uc.failedIntentApply(expected, i.Version, "runtime_stage_failed", err)
	} else if err != nil {
		logging.Debugf("pause applied to DB-only torrent hash=%s", hash)
	}

	_, err = uc.runIntentCommand(i.Version, true, true, func() (models.IntentVersion, error) {
		return uc.repo.CommitRunIntent(i.Version, models.RunPaused)
	}, func() error { return uc.engine.ReconcilePausedIntent(t) })
	return err
}

func (uc *TorrentUseCase) Resume(hash string, contexts ...context.Context) error {
	release, err := uc.lockRecord(hash, contexts...)
	if err != nil {
		return err
	}
	defer release()
	return uc.resumeLocked(hash)
}

func (uc *TorrentUseCase) resumeLocked(hash string) error {
	logging.Infof("usecase resume hash=%s", hash)
	if err := uc.repairMissingInventory(hash); err != nil {
		return err
	}
	t, err := uc.repo.GetTorrent(hash)
	if err != nil {
		logging.Warnf("resume failed to load torrent hash=%s: %v", hash, err)
		return err
	}
	if t == nil {
		logging.Debugf("resume requested for missing torrent hash=%s", hash)
		return TorrentNotFoundError{Hash: hash}
	}
	if err := uc.checkStorageAvailable(hash); err != nil && !uc.canFetchOnlyMetadata(t) {
		return err
	}
	if _, pending, err := uc.repo.CleanupPaths(hash); err != nil {
		return err
	} else if pending {
		return ErrStorageConflict
	}
	i, err := uc.currentIntent(hash)
	if err != nil {
		return err
	}
	blocks, err := uc.repo.ActivationBlocks(i.Version)
	if err != nil {
		return err
	}
	for _, block := range blocks {
		if !block.Kind.ResumeRetryable() {
			return ErrStorageConflict
		}
	}

	expected := uc.engine.GetRawTorrent(hash)
	if err := uc.engine.StageExistingRestore(t); err != nil {
		if !errors.Is(err, engine.ErrTorrentNotFound) {
			logging.Warnf("engine resume failed hash=%s: %v", hash, err)
			return uc.failedIntentApply(expected, i.Version, "runtime_stage_failed", err)
		}
		logging.Infof("resume restoring DB-only torrent hash=%s source_present=%t", hash, t.SourceURI != "")
		restored, err := uc.restoreTorrentToEngine(t)
		if err != nil {
			logging.Warnf("resume restore failed hash=%s: %v", hash, err)
			return err
		}
		if t.SourceURI == "" {
			t.SourceURI = restored.SourceURI
		}
	}

	_, err = uc.runIntentCommand(i.Version, true, true, func() (models.IntentVersion, error) {
		v, err := uc.repo.CommitResumeRetry(i.Version)
		if err != nil {
			_ = uc.engine.Pause(hash)
		}
		return v, err
	}, func() error { return uc.engine.FinishRestore(hash, models.StateQueued, true) })
	return err
}

func (uc *TorrentUseCase) SetFilePriority(hash string, fileIndex int, priority models.FilePriority, contexts ...context.Context) error {
	release, err := uc.lockRecord(hash, contexts...)
	if err != nil {
		return err
	}
	defer release()
	logging.Infof("usecase set file priority hash=%s index=%d priority=%d", hash, fileIndex, priority)

	if fileIndex < 0 || (priority != models.PriorityNormal && priority != models.PriorityHigh && priority != models.PriorityNone) {
		return ErrInvalidPriority
	}
	saved, err := uc.repo.GetTorrent(hash)
	if err != nil {
		return err
	}
	if saved == nil {
		return TorrentNotFoundError{Hash: hash}
	}
	if err := uc.priorityAvailable(hash); err != nil {
		return err
	}
	known := false
	for _, file := range saved.Files {
		if file != nil && file.Index == fileIndex {
			known = true
			break
		}
	}
	if !known {
		if len(saved.Files) == 0 {
			return ErrMetadataPending
		}
		return ErrFileNotFound
	}
	expected := uc.engine.GetRawTorrent(hash)
	// User intent is committed before changing the runtime. Statistics saves never
	// overwrite it, even if metadata is still pending.
	if err := uc.repo.UpdateFilePriority(hash, fileIndex, priority); err != nil {
		logging.Warnf("failed to persist file priority hash=%s index=%d: %v", hash, fileIndex, err)
		return err
	}
	v := nextSavedVersion(saved)
	return uc.applyCommittedIntent(expected, v, false, func() error { return uc.engine.SetFilePriority(hash, fileIndex, priority) })
}

func (uc *TorrentUseCase) SetTorrentFilesPriority(hash string, priority models.FilePriority, contexts ...context.Context) error {
	release, err := uc.lockRecord(hash, contexts...)
	if err != nil {
		return err
	}
	defer release()
	logging.Infof("usecase set torrent files priority hash=%s priority=%d", hash, priority)

	if priority != models.PriorityNormal && priority != models.PriorityHigh && priority != models.PriorityNone {
		return ErrInvalidPriority
	}
	saved, err := uc.repo.GetTorrent(hash)
	if err != nil {
		return err
	}
	if saved == nil {
		return TorrentNotFoundError{Hash: hash}
	}
	if err := uc.priorityAvailable(hash); err != nil {
		return err
	}
	if len(saved.Files) == 0 {
		return ErrMetadataPending
	}
	expected := uc.engine.GetRawTorrent(hash)
	if err := uc.repo.UpdateTorrentFilesPriority(hash, priority); err != nil {
		logging.Warnf("failed to persist torrent files priority hash=%s: %v", hash, err)
		return err
	}
	priorities := make(map[int]models.FilePriority, len(saved.Files))
	for _, file := range saved.Files {
		if file == nil {
			continue
		}
		priorities[file.Index] = priority
	}
	v := nextSavedVersion(saved)
	return uc.applyCommittedIntent(expected, v, false, func() error { return uc.engine.SetFilePriorities(hash, priorities) })
}

func (uc *TorrentUseCase) priorityAvailable(hash string) error {
	if err := uc.checkStorageWorkflow(hash); err != nil {
		return err
	}
	if _, pending, err := uc.repo.CleanupPaths(hash); err != nil {
		return err
	} else if pending {
		return ErrStorageConflict
	}
	return nil
}

func (uc *TorrentUseCase) Delete(hash string, deleteFiles bool, contexts ...context.Context) (retErr error) {
	release, err := uc.lockRecord(hash, contexts...)
	if err != nil {
		return err
	}
	defer release()
	logging.Infof("usecase delete hash=%s delete_files=%t", hash, deleteFiles)
	t, err := uc.repo.GetTorrent(hash)
	if err != nil {
		logging.Warnf("delete failed to load torrent hash=%s: %v", hash, err)
		return err
	}
	if t == nil {
		logging.Debugf("delete requested for missing torrent hash=%s", hash)
		return TorrentNotFoundError{Hash: hash}
	}
	if (t.DeletionMode == "keep" && deleteFiles) || (t.DeletionMode == "payload" && !deleteFiles) {
		return &StorageOperationError{Code: models.StorageDeletionMode, Cause: ErrStorageConflict, Hash: hash}
	}
	defer func() {
		if retErr != nil && !deleteFiles {
			retErr = errors.Join(retErr, uc.repo.SaveDeletionError(hash, retErr))
		}
	}()

	if uc.transferProtected(hash) {
		return ErrStorageConflict
	}
	if rec, err := uc.repo.Recovery(hash); err != nil {
		return err
	} else if recoveryBlocks(rec) && !(rec.Reason == models.StorageDeletionCancelled && rec.Stage == "cancelled") && rec.Reason != models.StorageDeletionPending {
		return ErrStorageConflict
	}
	if m, err := uc.repo.StorageMapping(hash); err != nil {
		return err
	} else if m != nil && (m.State == "moving" || m.Manifest != "") {
		return ErrStorageConflict
	}
	if deleteFiles {
		return uc.deletePayloadLocked(t)
	}
	if err := uc.repo.BeginForget(hash); err != nil {
		return err
	}
	if uc.beforeDetach != nil {
		uc.beforeDetach(hash)
	}
	if err := uc.engine.Delete(hash); err != nil {
		return err
	}
	if err := uc.repo.AdvanceDeletion(hash, t.Control.RecordID, "removing_record", ""); err != nil {
		return err
	}
	if err := uc.repo.DeletionGate(hash, t.Control.RecordID); err != nil {
		return err
	}

	// Delete from DB after optional file removal so a failed file delete remains visible.
	if err := uc.repo.DeleteTorrent(hash); err != nil {
		logging.Warnf("failed to delete torrent from DB hash=%s: %v", hash, err)
		return err
	}

	return nil
}

func validateRelativeDownloadPath(relPath string) error {
	return payload.ValidateRelative(relPath)
}

func (uc *TorrentUseCase) RestoreTorrents() error {
	return uc.RestoreTorrentsContext(uc.engine.Context())
}

func (uc *TorrentUseCase) RestoreTorrentsContext(ctx context.Context) error {
	defer func() {
		select {
		case uc.inventoryWake <- struct{}{}:
		default:
		}
	}()
	started := time.Now()
	logging.Infof("restoring persisted torrents stage=storage-init")
	if err := uc.initializeStorageLocked(ctx); err != nil {
		return err
	}
	torrents, err := uc.repo.GetAllTorrentsContext(ctx)
	if err != nil {
		logging.Warnf("failed to load torrents for restore: %v", err)
		return err
	}
	logging.Infof("restore loaded persisted torrents count=%d", len(torrents))

	var restoreErr error
	sort.Slice(torrents, func(i, j int) bool {
		if torrents[i].QueuePosition != torrents[j].QueuePosition {
			return torrents[i].QueuePosition < torrents[j].QueuePosition
		}
		return torrents[i].Hash < torrents[j].Hash
	})
	restored, skipped, failed := 0, 0, 0
	for index, t := range torrents {
		if err := ctx.Err(); err != nil {
			return errors.Join(restoreErr, err)
		}
		recordStarted := time.Now()
		logging.Infof("restore torrent starting hash=%s record=%d/%d", t.Hash, index+1, len(torrents))
		ok, err := uc.restorePersistedTorrent(ctx, t.Hash)
		if err != nil {
			failed++
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore torrent %s: %w", t.Hash, err))
			logging.Warnf("restore torrent failed hash=%s duration=%s: %v", t.Hash, time.Since(recordStarted), err)
		} else if ok {
			restored++
			logging.Infof("restore torrent finished hash=%s duration=%s", t.Hash, time.Since(recordStarted))
		} else {
			skipped++
			logging.Infof("restore torrent deferred hash=%s duration=%s", t.Hash, time.Since(recordStarted))
		}
	}
	logging.Infof("persisted torrent restore finished restored=%d deferred=%d failed=%d duration=%s", restored, skipped, failed, time.Since(started))
	return restoreErr
}

func (uc *TorrentUseCase) restorePersistedTorrent(ctx context.Context, hash string) (bool, error) {
	release, err := uc.lockRecord(hash, ctx)
	if err != nil {
		return false, err
	}
	defer release()
	current, err := uc.repo.GetTorrentContext(ctx, hash)
	if err != nil || current == nil {
		return false, err
	}
	if recovery, err := uc.repo.Recovery(hash); err != nil {
		return false, err
	} else if recovery != nil {
		if recovery.Stage == "cancel_requested" && recovery.Reason != models.StorageMove && recovery.Reason != models.StorageLocate {
			if err := uc.engine.Delete(hash); err != nil {
				return false, err
			}
			return false, uc.repo.CompleteDeletionCancel(hash, recovery.RecordID, recovery.Version)
		}
		if recoveryBlocks(recovery) {
			return false, nil
		}
	}
	if ready, err := uc.inspectInventory(ctx, hash, false); err != nil || !ready {
		return false, err
	}
	if err := uc.checkStorageAvailable(hash); err != nil && !uc.canFetchOnlyMetadata(current) {
		if errors.Is(err, ErrStorageConflict) {
			return false, nil
		}
		return false, err
	}
	if _, pending, err := uc.repo.CleanupPaths(hash); err != nil || pending {
		return false, err
	}
	err = uc.restoreMappedTorrentLocked(current)
	if errors.Is(err, ErrMetadataPending) {
		return false, nil
	}
	return err == nil, err
}

func (uc *TorrentUseCase) restoreTorrentToEngine(t *models.Torrent) (result *models.Torrent, retErr error) {
	defer func() {
		if retErr != nil {
			retErr = uc.attachmentFailure(t, retErr)
		}
	}()
	var restoreErr error
	path := filepath.Join(uc.engine.ConfigDir(), "metainfo", strings.ToLower(t.Hash)+".torrent")
	f, err := os.Open(path)
	if err == nil {
		logging.Debugf("restore uses persisted metainfo hash=%s", t.Hash)
		mi, addErr := metainfo.Load(f)
		closeErr := f.Close()
		if addErr == nil && closeErr == nil {
			spec, specErr := torrent.TorrentSpecFromMetaInfoErr(mi)
			if specErr != nil {
				return nil, specErr
			}
			info, infoErr := mi.UnmarshalInfo()
			if infoErr != nil {
				return nil, infoErr
			}
			if err := uc.prepareNamedStorage(uc.engine.Context(), t.Hash, &info); err != nil {
				return nil, err
			}
			return uc.engine.RestoreTorrent(spec, t)
		}
		if addErr == nil {
			addErr = closeErr
		}
		restoreErr = errors.Join(restoreErr, fmt.Errorf("metainfo_file: %w", addErr))
		logging.Warnf("restore metainfo failed hash=%s, trying fallback: %v", t.Hash, addErr)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		logging.Warnf("restore failed to open persisted metainfo hash=%s: %v", t.Hash, err)
		restoreErr = errors.Join(restoreErr, fmt.Errorf("open metainfo_file: %w", err))
	}

	if t.SourceURI != "" {
		logging.Debugf("restore uses persisted source URI hash=%s", t.Hash)
		spec, err := torrent.TorrentSpecFromMagnetUri(t.SourceURI)
		if err == nil {
			return uc.engine.RestoreTorrent(spec, t)
		}
		restoreErr = errors.Join(restoreErr, fmt.Errorf("magnet: %w", err))
		logging.Warnf("restore magnet failed hash=%s, trying infohash fallback: %v", t.Hash, err)
	}

	logging.Warnf("restore falling back to bare info hash hash=%s", t.Hash)
	spec, err := torrent.TorrentSpecFromMagnetUri("magnet:?xt=urn:btih:" + t.Hash)
	if err == nil {
		return uc.engine.RestoreTorrent(spec, t)
	}
	restoreErr = errors.Join(restoreErr, fmt.Errorf("infohash: %w", err))
	return nil, restoreErr
}

func (uc *TorrentUseCase) GetCacheStatus(hash string, index int, offset int64) (*engine.CacheStatus, error) {
	return uc.engine.GetCacheStatus(hash, index, offset)
}

func (uc *TorrentUseCase) Warmup(ctx context.Context, hash string, index int, size int64) (int64, int64, error) {
	return uc.engine.Warmup(ctx, hash, index, size)
}

func (uc *TorrentUseCase) StartTask(fn func(context.Context)) bool { return uc.engine.StartTask(fn) }
func (uc *TorrentUseCase) GenerationContext(hash string) (context.Context, error) {
	return uc.engine.GenerationContext(hash)
}

func (uc *TorrentUseCase) AddStream(ctx context.Context, hash string, index int, opts StreamOptions) error {
	logging.Debugf("usecase add stream hash=%s file_index=%d range_start=%d has_range=%t head=%t skip=%t", hash, index, opts.RangeStart, opts.HasRange, opts.IsHEAD, opts.SkipQoS)
	if err := uc.engine.StreamManager().AddStream(ctx, hash, index, opts); err != nil {
		if errors.Is(err, engine.ErrTorrentNotFound) {
			logging.Debugf("stream requested for missing torrent hash=%s file_index=%d", hash, index)
			return TorrentNotFoundError{Hash: hash}
		}
		logging.Warnf("failed to add stream hash=%s file_index=%d: %v", hash, index, err)
		return err
	}
	return nil
}

func (uc *TorrentUseCase) OpenStreamSource(ctx context.Context, hash string, index int, isHEAD bool) (*engine.StreamSource, error) {
	release, err := uc.lockRecords(ctx, hash)
	if err != nil {
		return nil, err
	}
	err = uc.prepareReadLocked(ctx, hash, index)
	release()
	if err != nil {
		return nil, err
	}
	source, err := uc.engine.OpenStreamSource(ctx, hash, index, isHEAD)
	if err != nil {
		var notFound engine.TorrentNotFoundError
		if errors.Is(err, engine.ErrTorrentNotFound) || errors.As(err, &notFound) {
			return nil, TorrentNotFoundError{Hash: hash}
		}
		return nil, err
	}
	return source, nil
}

func (uc *TorrentUseCase) CheckFileDirectReady(hash string, index int) (bool, int64, error) {
	ready, length, err := uc.engine.CheckFileDirectReady(hash, index)
	if err != nil {
		var notFound engine.TorrentNotFoundError
		if errors.Is(err, engine.ErrTorrentNotFound) || errors.As(err, &notFound) {
			return false, 0, TorrentNotFoundError{Hash: hash}
		}
		return false, 0, err
	}
	return ready, length, nil
}

func (uc *TorrentUseCase) GetDownloadedFile(hash string, index int, contexts ...context.Context) (*models.Torrent, DownloadedDiskFile, error) {
	release, err := uc.lockRecord(hash, contexts...)
	if err != nil {
		return nil, DownloadedDiskFile{}, err
	}
	defer release()
	t, err := uc.GetTorrent(hash)
	if err != nil {
		return nil, DownloadedDiskFile{}, err
	}
	if t == nil {
		return nil, DownloadedDiskFile{}, TorrentNotFoundError{Hash: hash}
	}
	if len(t.Files) == 0 {
		return nil, DownloadedDiskFile{}, ErrDownloadNotReady
	}

	for _, file := range t.Files {
		if file == nil || file.Index != index {
			continue
		}
		diskFile, err := uc.resolveDownloadedFile(uc.requestContext(contexts...), t, file)
		if err != nil {
			return nil, DownloadedDiskFile{}, err
		}
		return t, diskFile, nil
	}

	return nil, DownloadedDiskFile{}, os.ErrNotExist
}

func (uc *TorrentUseCase) GetDownloadedTorrentFiles(hash string, contexts ...context.Context) (*models.Torrent, []DownloadedDiskFile, error) {
	release, err := uc.lockRecord(hash, contexts...)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	t, err := uc.GetTorrent(hash)
	if err != nil {
		return nil, nil, err
	}
	if t == nil {
		return nil, nil, TorrentNotFoundError{Hash: hash}
	}
	if len(t.Files) == 0 {
		return nil, nil, ErrDownloadNotReady
	}

	files := make([]DownloadedDiskFile, 0, len(t.Files))
	for _, file := range t.Files {
		if file == nil {
			continue
		}
		diskFile, err := uc.resolveDownloadedFile(uc.requestContext(contexts...), t, file)
		if err != nil {
			return nil, nil, err
		}
		files = append(files, diskFile)
	}
	if len(files) == 0 {
		return nil, nil, ErrDownloadNotReady
	}

	return t, files, nil
}

func (uc *TorrentUseCase) resolveDownloadedFile(ctx context.Context, t *models.Torrent, file *models.File) (DownloadedDiskFile, error) {
	if err := uc.checkStorageAvailable(t.Hash); err != nil {
		return DownloadedDiskFile{}, err
	}
	if file == nil || file.Path == "" {
		return DownloadedDiskFile{}, ErrDownloadNotReady
	}
	if err := uc.prepareReadLocked(ctx, t.Hash, file.Index); err != nil {
		return DownloadedDiskFile{}, err
	}
	info, err := uc.metadataStorageInfo(t)
	if err != nil {
		return DownloadedDiskFile{}, err
	}
	if info == nil {
		return DownloadedDiskFile{}, ErrStorageConflict
	}
	paths, _, err := uc.metadataStoragePaths(t)
	if err != nil || file.Index < 0 || file.Index >= len(paths) {
		return DownloadedDiskFile{}, ErrStorageConflict
	}
	if info.UpvertedFiles()[file.Index].Length != file.Size {
		return DownloadedDiskFile{}, ErrStorageConflict
	}
	path := paths[file.Index]
	root, err := os.OpenRoot(uc.engine.DownloadDir())
	if err != nil {
		return DownloadedDiskFile{}, err
	}
	_, statErr := root.Lstat(path)
	root.Close()
	if errors.Is(statErr, os.ErrNotExist) {
		return DownloadedDiskFile{}, os.ErrNotExist
	}
	in, err := uc.openPayloadLocked(t.Hash, file.Index, path, file.Size)
	if err != nil {
		return DownloadedDiskFile{}, err
	}
	_ = in.Close()
	if err := validateRelativeDownloadPath(file.Path); err != nil {
		return DownloadedDiskFile{}, ErrStorageConflict
	}
	hash, size := t.Hash, file.Size
	return DownloadedDiskFile{Index: file.Index, Path: file.Path, Size: size, open: func() (*os.File, error) {
		release, err := uc.lockRecord(hash)
		if err != nil {
			return nil, err
		}
		defer release()
		return uc.openPayloadLocked(hash, file.Index, path, size)
	}}, nil
}

func applyMetadata(t *models.Torrent, metadata TorrentMetadata) {
	if t == nil {
		return
	}
	if metadata.Title != "" {
		t.Title = metadata.Title
	}
	if metadata.Data != "" {
		t.Data = metadata.Data
	}
	if metadata.Poster != "" {
		t.Poster = metadata.Poster
	}
	if metadata.Category != "" {
		t.Category = metadata.Category
	}
}

func mergePersistedMetadata(runtime, persisted *models.Torrent) {
	if runtime == nil || persisted == nil {
		return
	}
	runtime.CatalogVersion = persisted.CatalogVersion
	runtime.IsTemporary = persisted.IsTemporary
	runtime.DeletionMode, runtime.DeletionError = persisted.DeletionMode, persisted.DeletionError
	if persisted.DeletionMode != "" {
		runtime.State = models.StatePaused
	}
	runtime.StorageState, runtime.StorageError = persisted.StorageState, persisted.StorageError
	runtime.StorageRecovery = persisted.StorageRecovery
	runtime.Storage = persisted.Storage
	runtime.Title = persisted.Title
	runtime.Data = persisted.Data
	runtime.Poster = persisted.Poster
	runtime.Category = persisted.Category
	runtime.SourceURI = firstNonEmpty(runtime.SourceURI, persisted.SourceURI)
	runtime.LegacyDownloaded = persisted.LegacyDownloaded
	if !runtime.MetadataKnown {
		// Runtime metadata-pending is not evidence that previously known fields
		// vanished. Expose their provenance without promoting them to readiness.
		runtime.Size, runtime.Downloaded, runtime.Progress = persisted.Size, persisted.Downloaded, persisted.Progress
		runtime.ProgressSource, runtime.ProgressObservedAt = persisted.ProgressSource, persisted.ProgressObservedAt
		runtime.MetadataKnown, runtime.PieceLength, runtime.NumPieces = persisted.MetadataKnown, persisted.PieceLength, persisted.NumPieces
		runtime.Files = make([]*models.File, 0, len(persisted.Files))
		for _, file := range persisted.Files {
			if file != nil {
				copy := *file
				runtime.Files = append(runtime.Files, &copy)
			}
		}
	} else {
		legacy := make(map[int]*models.File, len(persisted.Files))
		for _, file := range persisted.Files {
			if file != nil {
				legacy[file.Index] = file
			}
		}
		for _, file := range runtime.Files {
			if file != nil && legacy[file.Index] != nil {
				file.LegacyDownloaded = legacy[file.Index].LegacyDownloaded
				file.Priority = legacy[file.Index].Priority
			}
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (uc *TorrentUseCase) MetainfoSettings() (string, string) { return uc.engine.MetainfoSettings() }
