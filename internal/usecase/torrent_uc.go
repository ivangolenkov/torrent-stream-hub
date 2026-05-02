package usecase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"

	"github.com/anacrolix/torrent"
)

var ErrTorrentNotFound = errors.New("torrent not found")

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
	engine *engine.Engine
	repo   *repository.TorrentRepo
}

type TorrentMetadata struct {
	Title    string
	Data     string
	Poster   string
	Category string
}

func NewTorrentUseCase(e *engine.Engine, r *repository.TorrentRepo) *TorrentUseCase {
	return &TorrentUseCase{
		engine: e,
		repo:   r,
	}
}

func (uc *TorrentUseCase) AddMagnet(magnet string) (*models.Torrent, error) {
	return uc.AddMagnetWithMetadata(magnet, TorrentMetadata{})
}

func (uc *TorrentUseCase) AddMagnetWithMetadata(magnet string, metadata TorrentMetadata) (*models.Torrent, error) {
	logging.Infof("usecase add magnet %s", logging.SafeMagnetSummary(magnet))
	t, err := uc.engine.AddMagnet(magnet)
	if err != nil {
		logging.Warnf("usecase add magnet failed %s: %v", logging.SafeMagnetSummary(magnet), err)
		return nil, err
	}

	applyMetadata(t, metadata)

	// Save initial state to DB
	if err := uc.repo.SaveTorrent(t); err != nil {
		logging.Warnf("failed to persist torrent after magnet add hash=%s: %v", t.Hash, err)
		return nil, err
	}

	return t, nil
}

func (uc *TorrentUseCase) AddTorrentFile(r io.Reader) (*models.Torrent, error) {
	logging.Infof("usecase add .torrent file")
	t, err := uc.engine.AddTorrentFile(r)
	if err != nil {
		logging.Warnf("usecase add .torrent file failed: %v", err)
		return nil, err
	}

	if err := uc.repo.SaveTorrent(t); err != nil {
		logging.Warnf("failed to persist torrent after file add hash=%s: %v", t.Hash, err)
		return nil, err
	}

	return t, nil
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

func (uc *TorrentUseCase) HardRefresh(hash string) error {
	logging.Infof("usecase hard refresh hash=%s", hash)
	if err := uc.engine.HardRefresh(hash, "manual api action"); err != nil {
		if errors.Is(err, engine.ErrTorrentNotFound) {
			return TorrentNotFoundError{Hash: hash}
		}
		return err
	}
	return nil
}

func (uc *TorrentUseCase) RecycleBTClient() error {
	logging.Infof("usecase recycle bt client")
	return uc.engine.RecycleClient("manual api action")
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

func (uc *TorrentUseCase) UpdateMetadata(hash string, metadata TorrentMetadata) (*models.Torrent, error) {
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

	if err := uc.engine.Resume(hash); err != nil {
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
		logging.Warnf("failed to persist resumed state hash=%s: %v", hash, err)
		return err
	}
	return nil
}

func (uc *TorrentUseCase) Delete(hash string, deleteFiles bool) error {
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

	engineTorrent := uc.engine.GetTorrent(hash)
	if engineTorrent != nil && len(engineTorrent.Files) > len(t.Files) {
		mergePersistedMetadata(engineTorrent, t)
		t = engineTorrent
	}

	if err := uc.engine.Delete(hash); err != nil {
		logging.Warnf("engine delete failed hash=%s: %v", hash, err)
		return err
	}

	if deleteFiles {
		if err := uc.deleteTorrentFiles(t); err != nil {
			logging.Warnf("failed to delete torrent files hash=%s: %v", hash, err)
			return err
		}
	}

	// Delete from DB after optional file removal so a failed file delete remains visible.
	if err := uc.repo.DeleteTorrent(hash); err != nil {
		logging.Warnf("failed to delete torrent from DB hash=%s: %v", hash, err)
		return err
	}

	return nil
}

func (uc *TorrentUseCase) deleteTorrentFiles(t *models.Torrent) error {
	if t == nil {
		return nil
	}
	downloadDir := filepath.Clean(uc.engine.DownloadDir())
	deleted := false
	for _, file := range t.Files {
		if file == nil || file.Path == "" {
			continue
		}
		if err := validateRelativeDownloadPath(file.Path); err != nil {
			return err
		}
		for _, path := range torrentFilePathCandidates(downloadDir, t.Name, file.Path) {
			if err := removeTorrentFilePath(path, downloadDir); err != nil {
				return err
			}
			deleted = true
		}
	}

	if !deleted && t.Name != "" {
		path, err := safeDownloadPath(downloadDir, t.Name)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Remove(path + ".part"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func torrentFilePathCandidates(downloadDir, torrentName, filePath string) []string {
	candidates := make([]string, 0, 2)
	if path, err := safeDownloadPath(downloadDir, filePath); err == nil {
		candidates = append(candidates, path)
	}
	if torrentName != "" {
		if path, err := safeDownloadPath(downloadDir, filepath.Join(torrentName, filePath)); err == nil && !containsPath(candidates, path) {
			candidates = append(candidates, path)
		}
	}
	return candidates
}

func removeTorrentFilePath(path, downloadDir string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(path + ".part"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	pruneEmptyDirs(filepath.Dir(path), downloadDir)
	return nil
}

func safeDownloadPath(downloadDir, relPath string) (string, error) {
	if err := validateRelativeDownloadPath(relPath); err != nil {
		return "", err
	}
	path := filepath.Clean(filepath.Join(downloadDir, relPath))
	if path == downloadDir || !strings.HasPrefix(path, downloadDir+string(os.PathSeparator)) {
		return "", fmt.Errorf("refusing to delete path outside download dir: %s", relPath)
	}
	return path, nil
}

func validateRelativeDownloadPath(relPath string) error {
	clean := filepath.Clean(relPath)
	if clean == "." || clean == ".." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("refusing to delete path outside download dir: %s", relPath)
	}
	return nil
}

func containsPath(paths []string, path string) bool {
	for _, existing := range paths {
		if existing == path {
			return true
		}
	}
	return false
}

func pruneEmptyDirs(dir, stop string) {
	for dir != stop && strings.HasPrefix(dir, stop+string(os.PathSeparator)) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

func (uc *TorrentUseCase) RestoreTorrents() error {
	logging.Infof("restoring persisted torrents")
	torrents, err := uc.repo.GetAllTorrents()
	if err != nil {
		logging.Warnf("failed to load torrents for restore: %v", err)
		return err
	}

	var restoreErr error
	for _, t := range torrents {
		switch t.State {
		case models.StatePaused, models.StateError, models.StateMissingFiles:
			logging.Debugf("skipping restore hash=%s state=%s", t.Hash, t.State)
			continue
		}

		logging.Infof("restoring torrent hash=%s state=%s source_present=%t", t.Hash, t.State, t.SourceURI != "")
		restored, err := uc.restoreTorrentToEngine(t)
		if err == nil {
			mergePersistedMetadata(restored, t)
			if err := uc.repo.SaveTorrent(restored); err != nil {
				logging.Warnf("failed to save restored torrent hash=%s: %v", t.Hash, err)
				restoreErr = errors.Join(restoreErr, fmt.Errorf("save restored torrent %s: %w", t.Hash, err))
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
	if path := uc.engine.MetainfoPath(t.Hash); path != "" {
		f, err := os.Open(path)
		if err == nil {
			defer f.Close()
			logging.Debugf("restore uses persisted metainfo hash=%s", t.Hash)
			return uc.engine.AddTorrentFile(f)
		}
		if !errors.Is(err, os.ErrNotExist) {
			logging.Warnf("restore failed to open persisted metainfo hash=%s: %v", t.Hash, err)
		}
	}
	if t.SourceURI != "" {
		logging.Debugf("restore uses persisted source URI hash=%s", t.Hash)
		return uc.engine.AddMagnet(t.SourceURI)
	}

	logging.Warnf("restore falling back to bare info hash hash=%s", t.Hash)
	return uc.engine.AddInfoHash(t.Hash)
}

func (uc *TorrentUseCase) GetCacheStatus(hash string, index int, offset int64) (*engine.CacheStatus, error) {
	return uc.engine.GetCacheStatus(hash, index, offset)
}

func (uc *TorrentUseCase) Warmup(ctx context.Context, hash string, index int, size int64) (int64, int64, error) {
	return uc.engine.Warmup(ctx, hash, index, size)
}

func (uc *TorrentUseCase) AddStream(ctx context.Context, hash string, index int) error {
	logging.Debugf("usecase add stream hash=%s file_index=%d", hash, index)
	if err := uc.engine.StreamManager().AddStream(ctx, hash, index); err != nil {
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
