package usecase

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

var ErrTorrentNotFound = errors.New("torrent not found")
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
	engine               *engine.Engine
	repo                 *repository.TorrentRepo
	maxTemporary         int
	transfers            map[string]int
	protectedUntil       map[string]time.Time
	storageWorkerStarted bool
}

type StreamOptions = engine.StreamOptions

type TorrentMetadata struct {
	Title    string
	Data     string
	Poster   string
	Category string
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
	e.SetStorageRootResolver(func(hash string) (string, error) {
		m, err := r.StorageMapping(hash)
		if err != nil {
			return "", err
		}
		if m == nil || m.State != "ready" || m.Manifest != "" || m.Version != payload.LayoutVersion {
			return "", ErrStorageConflict
		}
		return m.Root, nil
	}, r.ClaimStorageAlias)
	return &TorrentUseCase{
		engine:         e,
		repo:           r,
		maxTemporary:   3,
		transfers:      make(map[string]int),
		protectedUntil: make(map[string]time.Time),
	}
}

func (uc *TorrentUseCase) AddMagnet(magnet string) (*models.Torrent, error) {
	return uc.AddMagnetWithMetadata(magnet, TorrentMetadata{})
}

func (uc *TorrentUseCase) AddMagnetWithMetadata(magnet string, metadata TorrentMetadata) (*models.Torrent, error) {
	return uc.AddMagnetWithPolicy(magnet, metadata, false)
}

func (uc *TorrentUseCase) AddMagnetWithPolicy(magnet string, metadata TorrentMetadata, temporary bool) (*models.Torrent, error) {
	uc.repo.Lifecycle.Lock()
	defer uc.repo.Lifecycle.Unlock()
	logging.Infof("usecase add magnet %s", logging.SafeMagnetSummary(magnet))
	parsed, err := torrent.TorrentSpecFromMagnetUri(magnet)
	if err != nil {
		return nil, err
	}
	if err := uc.checkSpecAddAllowed(parsed); err != nil {
		return nil, err
	}
	t, err := uc.engine.AddMagnet(magnet)
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
	uc.repo.Lifecycle.Lock()
	defer uc.repo.Lifecycle.Unlock()
	logging.Infof("usecase add .torrent file")
	mi, err := metainfo.Load(r)
	if err != nil {
		return nil, err
	}
	spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		return nil, err
	}
	if err := uc.checkSpecAddAllowed(spec); err != nil {
		return nil, err
	}
	var encoded bytes.Buffer
	if err := mi.Write(&encoded); err != nil {
		return nil, err
	}
	t, err := uc.engine.AddTorrentFile(&encoded)
	if err != nil {
		logging.Warnf("usecase add .torrent file failed: %v", err)
		return nil, err
	}

	return uc.finishAddLocked(t, metadata, temporary)
}

// Caller holds Lifecycle; reject re-add before engine mutation/admission.
func (uc *TorrentUseCase) checkSpecAddAllowed(spec *torrent.TorrentSpec) error {
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
	var alias string
	if spec.InfoHashV2.Ok {
		v2 := spec.InfoHashV2.Value
		alias = v2.ToShort().HexString()
	}
	return uc.reserveStorageLocked(hash, alias)
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
	dbTorrents, err := uc.repo.GetAllTorrents()
	if err != nil {
		return nil, err
	}

	engineTorrents := uc.engine.GetAllTorrents()
	if len(engineTorrents) == 0 {
		return dbTorrents, nil
	}

	engineByHash := make(map[string]*models.Torrent, len(engineTorrents))
	for _, t := range engineTorrents {
		engineByHash[t.Hash] = t
	}

	merged := make([]*models.Torrent, 0, len(dbTorrents)+len(engineTorrents))
	seen := make(map[string]bool, len(dbTorrents)+len(engineTorrents))
	for _, dbT := range dbTorrents {
		if engineT, ok := engineByHash[dbT.Hash]; ok {
			mergePersistedMetadata(engineT, dbT)
			merged = append(merged, engineT)
		} else {
			merged = append(merged, dbT)
		}
		seen[dbT.Hash] = true
	}

	for _, engineT := range engineTorrents {
		if !seen[engineT.Hash] {
			merged = append(merged, engineT)
		}
	}

	return merged, nil
}

func (uc *TorrentUseCase) BTHealth() *models.BTHealth {
	return uc.engine.BTHealth()
}

func (uc *TorrentUseCase) GetTorrent(hash string) (*models.Torrent, error) {
	dbT, err := uc.repo.GetTorrent(hash)
	if err != nil {
		return nil, err
	}
	engineT := uc.engine.GetTorrent(hash)
	if engineT == nil {
		return dbT, nil
	}
	mergePersistedMetadata(engineT, dbT)
	return engineT, nil
}

// GetTorrentPieces returns a string representing the status of all pieces in a torrent.
// "2" = downloaded, "1" = partial/downloading, "0" = missing.
// Returns an empty string if torrent or metadata is not available.
func (uc *TorrentUseCase) GetTorrentPieces(hash string) (string, error) {
	btTorrent := uc.engine.GetRawTorrent(hash)
	if btTorrent == nil {
		return "", TorrentNotFoundError{Hash: hash}
	}

	info := btTorrent.Info()
	if info == nil {
		return "", nil // metadata not ready
	}

	numPieces := info.NumPieces()
	if numPieces <= 0 {
		return "", nil
	}

	var sb strings.Builder
	sb.Grow(numPieces)

	runs := btTorrent.PieceStateRuns()
	for _, run := range runs {
		status := '0'
		if run.Complete {
			status = '2' // complete
		} else if run.Partial || run.Hashing || run.Checking || run.Priority != 0 {
			status = '1' // partial or downloading
		}

		for i := 0; i < run.Length; i++ {
			sb.WriteRune(status)
		}
	}

	return sb.String(), nil
}

func (uc *TorrentUseCase) UpdateMetadata(hash string, metadata TorrentMetadata) (*models.Torrent, error) {
	uc.repo.Lifecycle.Lock()
	defer uc.repo.Lifecycle.Unlock()
	t, err := uc.repo.GetTorrent(hash)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, TorrentNotFoundError{Hash: hash}
	}
	applyMetadata(t, metadata)
	if err := uc.repo.SaveTorrent(t); err != nil {
		return nil, err
	}
	return uc.GetTorrent(hash)
}

func (uc *TorrentUseCase) Pause(hash string) error {
	uc.repo.Lifecycle.Lock()
	defer uc.repo.Lifecycle.Unlock()
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

	if err := uc.engine.Pause(hash); err != nil && !errors.Is(err, engine.ErrTorrentNotFound) {
		logging.Warnf("engine pause failed hash=%s: %v", hash, err)
		return err
	} else if err != nil {
		logging.Debugf("pause applied to DB-only torrent hash=%s", hash)
	}

	t.State = models.StatePaused
	if err := uc.repo.SaveTorrent(t); err != nil {
		logging.Warnf("failed to persist paused state hash=%s: %v", hash, err)
		return err
	}
	return nil
}

func (uc *TorrentUseCase) Resume(hash string) error {
	uc.repo.Lifecycle.Lock()
	defer uc.repo.Lifecycle.Unlock()
	logging.Infof("usecase resume hash=%s", hash)
	t, err := uc.repo.GetTorrent(hash)
	if err != nil {
		logging.Warnf("resume failed to load torrent hash=%s: %v", hash, err)
		return err
	}
	if t == nil {
		logging.Debugf("resume requested for missing torrent hash=%s", hash)
		return TorrentNotFoundError{Hash: hash}
	}
	if err := uc.checkStorageAvailable(hash); err != nil {
		return err
	}
	if _, pending, err := uc.repo.CleanupPaths(hash); err != nil {
		return err
	} else if pending {
		return ErrStorageConflict
	}

	if err := uc.engine.StageExistingRestore(t); err != nil {
		if !errors.Is(err, engine.ErrTorrentNotFound) {
			logging.Warnf("engine resume failed hash=%s: %v", hash, err)
			return err
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

	t.State = models.StateQueued
	t.Error = models.ErrNone
	if err := uc.repo.SaveTorrent(t); err != nil {
		_ = uc.engine.Pause(hash)
		logging.Warnf("failed to persist resumed state hash=%s: %v", hash, err)
		return err
	}
	if err := uc.engine.FinishRestore(hash, models.StateQueued, true); err != nil {
		return err
	}
	return nil
}

func (uc *TorrentUseCase) SetFilePriority(hash string, fileIndex int, priority models.FilePriority) error {
	uc.repo.Lifecycle.Lock()
	defer uc.repo.Lifecycle.Unlock()
	logging.Infof("usecase set file priority hash=%s index=%d priority=%d", hash, fileIndex, priority)

	if fileIndex < 0 || (priority != models.PriorityNormal && priority != models.PriorityHigh && priority != models.PriorityNone) {
		return fmt.Errorf("invalid file priority")
	}
	saved, err := uc.repo.GetTorrent(hash)
	if err != nil {
		return err
	}
	if saved == nil {
		return TorrentNotFoundError{Hash: hash}
	}
	known := false
	for _, file := range saved.Files {
		if file != nil && file.Index == fileIndex {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("file index out of bounds or metadata not ready")
	}
	// User intent is committed before changing the runtime. Statistics saves never
	// overwrite it, even if metadata is still pending.
	if err := uc.repo.UpdateFilePriority(hash, fileIndex, priority); err != nil {
		logging.Warnf("failed to persist file priority hash=%s index=%d: %v", hash, fileIndex, err)
		return err
	}
	if err := uc.engine.SetFilePriority(hash, fileIndex, priority); err != nil && !errors.Is(err, engine.ErrTorrentNotFound) {
		return err
	}

	return nil
}

func (uc *TorrentUseCase) SetTorrentFilesPriority(hash string, priority models.FilePriority) error {
	uc.repo.Lifecycle.Lock()
	defer uc.repo.Lifecycle.Unlock()
	logging.Infof("usecase set torrent files priority hash=%s priority=%d", hash, priority)

	if priority != models.PriorityNormal && priority != models.PriorityHigh && priority != models.PriorityNone {
		return fmt.Errorf("invalid file priority")
	}
	saved, err := uc.repo.GetTorrent(hash)
	if err != nil {
		return err
	}
	if saved == nil {
		return TorrentNotFoundError{Hash: hash}
	}
	if len(saved.Files) == 0 {
		return fmt.Errorf("metadata not ready")
	}
	if err := uc.repo.UpdateTorrentFilesPriority(hash, priority); err != nil {
		logging.Warnf("failed to persist torrent files priority hash=%s: %v", hash, err)
		return err
	}
	for _, file := range saved.Files {
		if file == nil {
			continue
		}
		if err := uc.engine.SetFilePriority(hash, file.Index, priority); err != nil && !errors.Is(err, engine.ErrTorrentNotFound) {
			return err
		}
	}

	return nil
}

func (uc *TorrentUseCase) Delete(hash string, deleteFiles bool) error {
	uc.repo.Lifecycle.Lock()
	defer uc.repo.Lifecycle.Unlock()
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

	if uc.transferProtected(hash) {
		return ErrStorageConflict
	}
	if m, err := uc.repo.StorageMapping(hash); err != nil {
		return err
	} else if m != nil && m.State == "moving" {
		return ErrStorageConflict
	}
	if deleteFiles {
		return uc.deletePayloadLocked(t)
	}
	if err := uc.engine.Delete(hash); err != nil {
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
	uc.repo.Lifecycle.Lock()
	defer uc.repo.Lifecycle.Unlock()
	if err := uc.initializeStorageLocked(); err != nil {
		return err
	}
	logging.Infof("restoring persisted torrents")
	torrents, err := uc.repo.GetAllTorrents()
	if err != nil {
		logging.Warnf("failed to load torrents for restore: %v", err)
		return err
	}

	var restoreErr error
	for _, t := range torrents {
		if err := uc.checkStorageAvailable(t.Hash); err != nil {
			continue
		}
		if _, pending, err := uc.repo.CleanupPaths(t.Hash); err != nil {
			return err
		} else if pending {
			continue
		}
		switch t.State {
		case models.StatePaused, models.StateError, models.StateMissingFiles:
			if err := uc.restoreMappedTorrentLocked(t); err != nil {
				restoreErr = errors.Join(restoreErr, err)
			}
			continue
		}

		logging.Infof("restoring torrent hash=%s state=%s source_present=%t", t.Hash, t.State, t.SourceURI != "")
		restored, err := uc.restoreTorrentToEngine(t)
		if err == nil {
			mergePersistedMetadata(restored, t)
			if err := uc.repo.SaveTorrent(t); err != nil {
				logging.Warnf("failed to save restored torrent hash=%s: %v", t.Hash, err)
				restoreErr = errors.Join(restoreErr, fmt.Errorf("save restored torrent %s: %w", t.Hash, err))
				continue
			}
			if err := uc.engine.FinishRestore(t.Hash, t.State); err != nil {
				restoreErr = errors.Join(restoreErr, err)
			}
			continue
		}
		if err != nil {
			logging.Warnf("failed to restore torrent hash=%s: %v", t.Hash, err)
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore torrent %s: %w", t.Hash, err))
		}
	}

	return restoreErr
}

func (uc *TorrentUseCase) restoreTorrentToEngine(t *models.Torrent) (*models.Torrent, error) {
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

func (uc *TorrentUseCase) GetTorrentFile(hash string, index int) (*torrent.File, error) {
	logging.Debugf("usecase get torrent file hash=%s file_index=%d", hash, index)
	f, err := uc.engine.GetTorrentFile(hash, index)
	if err != nil {
		if errors.Is(err, engine.ErrTorrentNotFound) {
			logging.Debugf("get torrent file missing torrent hash=%s file_index=%d", hash, index)
			return nil, TorrentNotFoundError{Hash: hash}
		}
		logging.Warnf("get torrent file failed hash=%s file_index=%d: %v", hash, index, err)
		return nil, err
	}
	return f, nil
}

func (uc *TorrentUseCase) OpenStreamSource(ctx context.Context, hash string, index int, isHEAD bool) (*engine.StreamSource, error) {
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

func (uc *TorrentUseCase) GetDownloadedFile(hash string, index int) (*models.Torrent, DownloadedDiskFile, error) {
	uc.repo.Lifecycle.Lock()
	defer uc.repo.Lifecycle.Unlock()
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
		diskFile, err := uc.resolveDownloadedFile(t, file)
		if err != nil {
			return nil, DownloadedDiskFile{}, err
		}
		return t, diskFile, nil
	}

	return nil, DownloadedDiskFile{}, os.ErrNotExist
}

func (uc *TorrentUseCase) GetDownloadedTorrentFiles(hash string) (*models.Torrent, []DownloadedDiskFile, error) {
	uc.repo.Lifecycle.Lock()
	defer uc.repo.Lifecycle.Unlock()
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
		diskFile, err := uc.resolveDownloadedFile(t, file)
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

func (uc *TorrentUseCase) resolveDownloadedFile(t *models.Torrent, file *models.File) (DownloadedDiskFile, error) {
	if err := uc.checkStorageAvailable(t.Hash); err != nil {
		return DownloadedDiskFile{}, err
	}
	if file == nil || file.Path == "" {
		return DownloadedDiskFile{}, ErrDownloadNotReady
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
		uc.repo.Lifecycle.Lock()
		defer uc.repo.Lifecycle.Unlock()
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
	runtime.IsTemporary = persisted.IsTemporary
	runtime.StorageState, runtime.StorageError = persisted.StorageState, persisted.StorageError
	runtime.Title = firstNonEmpty(runtime.Title, persisted.Title)
	runtime.Data = firstNonEmpty(runtime.Data, persisted.Data)
	runtime.Poster = firstNonEmpty(runtime.Poster, persisted.Poster)
	runtime.Category = firstNonEmpty(runtime.Category, persisted.Category)
	runtime.SourceURI = firstNonEmpty(runtime.SourceURI, persisted.SourceURI)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
