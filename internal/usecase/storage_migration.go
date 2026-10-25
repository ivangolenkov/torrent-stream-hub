package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

type migrationEntry struct {
	Source string
	Target string
	Size   int64
	Digest string
	Copied bool
}
type migrationJournal struct {
	Root    string
	Entries []migrationEntry
}

func (uc *TorrentUseCase) initializeStorageLocked(contexts ...context.Context) error {
	ctx := uc.requestContext(contexts...)
	uc.storageInitMu.Lock()
	defer uc.storageInitMu.Unlock()
	all, err := uc.repo.GetAllTorrentsContext(ctx)
	if err != nil {
		return err
	}
	for _, t := range all {
		if err := ctx.Err(); err != nil {
			return err
		}
		m, err := uc.repo.StorageMapping(t.Hash)
		if err != nil {
			return err
		}
		if m == nil {
			if err := uc.repo.EnsureStorageMapping(&repository.StorageMapping{Hash: t.Hash, State: "legacy"}); err != nil {
				return err
			}
		}
	}
	if !uc.storageWorkerStarted {
		if !uc.engine.RunStorageTask(uc.runStorageMigrations) {
			return engine.ErrTaskLimit
		}
		uc.storageWorkerStarted = true
	}
	return nil
}

func (uc *TorrentUseCase) checkStorageAvailable(hash string) error {
	if recovery, err := uc.repo.Recovery(hash); err != nil {
		return err
	} else if recoveryBlocks(recovery) {
		return &StorageOperationError{Hash: hash, Code: recovery.Reason, Cause: ErrStorageConflict}
	}
	m, err := uc.repo.StorageMapping(hash)
	if err != nil {
		return err
	}
	if m == nil || m.Version != payload.LayoutVersion || m.State != "ready" || m.Manifest != "" {
		return ErrStorageConflict
	}
	return nil
}

func (uc *TorrentUseCase) reserveStorageLocked(hash, alias string) error {
	m, err := uc.repo.StorageMapping(hash)
	if err != nil {
		return err
	}
	if m == nil && alias != "" {
		m, err = uc.repo.StorageMapping(alias)
		if err != nil {
			return err
		}
		if m != nil && m.State == "ready" {
			copy := *m
			copy.Hash = hash
			if err := uc.publishStorage(&copy, true, false); err != nil {
				return err
			}
			m = &copy
		}
	}
	if m == nil {
		root, err := os.OpenRoot(uc.engine.DownloadDir())
		if err != nil {
			return err
		}
		defer root.Close()
		rel := payload.Root(hash)
		if _, err := root.Lstat(rel); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: isolated target already occupied", ErrStorageConflict)
		}
		ownershipRelease, err := uc.reserveOwnership(uc.engine.Context(), root, hash, []string{rel})
		if err != nil {
			return err
		}
		defer ownershipRelease()
		m = &repository.StorageMapping{Hash: hash, Version: payload.LayoutVersion, Root: rel, State: "ready"}
		if err := uc.publishStorage(m, false, true); err != nil {
			return err
		}
		actual, err := uc.repo.StorageMapping(hash)
		if err != nil {
			return err
		}
		if actual == nil || actual.Root != m.Root || actual.State != "ready" || actual.Manifest != "" {
			return ErrStorageConflict
		}
		m = actual
	}
	if m.State != "ready" || m.Version != payload.LayoutVersion || m.Manifest != "" {
		return ErrStorageConflict
	}
	uc.ownershipMu.Lock()
	claimErr := uc.claimsConflictLocked(hash, []string{m.Root})
	uc.ownershipMu.Unlock()
	if claimErr != nil {
		return claimErr
	}
	if alias != "" && alias != hash {
		a, err := uc.repo.StorageMapping(alias)
		if err != nil {
			return err
		}
		if a != nil && (a.Root != m.Root || a.State != "ready") {
			return ErrStorageConflict
		}
		if a == nil {
			copy := *m
			copy.Hash = alias
			if err := uc.publishStorage(&copy, true, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func (uc *TorrentUseCase) runStorageMigrations(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		all, err := uc.repo.GetAllTorrentsContext(ctx)
		if err == nil {
			uc.storageLiveMu.Lock()
			known := make(map[string]bool, len(all))
			for _, t := range all {
				known[t.Hash] = true
			}
			for hash := range uc.migrationFailures {
				if !known[hash] {
					delete(uc.migrationFailures, hash)
				}
			}
			uc.storageLiveMu.Unlock()
			for _, t := range all {
				if ctx.Err() != nil {
					return
				}
				err := uc.migrateStorage(ctx, t)
				if errors.Is(err, context.Canceled) {
					return
				}
				if errors.Is(err, errCoordinatorBusy) {
					continue
				}
				if uc.observeMigrationFailure(t, err) {
					uc.storageLiveMu.Lock()
					failure := uc.migrationFailures[t.Hash]
					uc.storageLiveMu.Unlock()
					logging.Warnf("storage migration hash=%s repeats=%d first_error=%v", t.Hash, failure.count, failure.first)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-uc.storageWake:
		}
	}
}

// Migration releases command handles during copying/hashing. Its work ticket and durable
// moving state rejects admission, re-add, Resume and deletion of this instance.
func (uc *TorrentUseCase) migrateStorage(ctx context.Context, t *models.Torrent) (retErr error) {
	hint, hintErr := uc.repo.StorageMapping(t.Hash)
	if hintErr != nil {
		return hintErr
	}
	if hint == nil || (hint.State == "ready" && hint.Manifest == "") {
		return nil
	}
	sequential, err := uc.coordinator.tryAcquire("storage/migration")
	if err != nil {
		return err
	}
	defer sequential.release()
	work, err := uc.coordinator.tryAcquire("migration/" + t.Hash)
	if err != nil {
		return err
	}
	defer work.release()
	release, err := uc.lockRecords(ctx, t.Hash)
	if err != nil {
		return err
	}
	current, err := uc.repo.GetTorrent(t.Hash)
	if err != nil || current == nil {
		release()
		return err
	}
	t = current
	recordID := t.Control.RecordID
	operationVersion := int64(1)
	m, err := uc.repo.StorageMapping(t.Hash)
	if err != nil || m == nil {
		release()
		return err
	}
	_, pending, err := uc.repo.CleanupPaths(t.Hash)
	if err != nil || pending {
		release()
		return err
	}
	if rec, recoveryErr := uc.repo.Recovery(t.Hash); recoveryErr != nil {
		release()
		return recoveryErr
	} else if recoveryBlocks(rec) {
		operationVersion = rec.Version
		authorization, _ := ctx.Value(cancelledMigrationKey{}).(cancelledMigrationAuthorization)
		if rec.Reason != models.StorageDeletionCancelled || rec.Stage != "verifying" || authorization.recordID != recordID || authorization.version != rec.Version {
			release()
			return nil
		}
	}
	if m.State == "ready" && m.Manifest == "" {
		release()
		return nil
	}
	stopProgress := uc.startStorageProgress(t.Hash, recordID, operationVersion, models.StorageMigrationActive)
	defer stopProgress()
	ctx = context.WithValue(ctx, migrationProgressKey{}, func(n int64) { uc.storageBytes(t.Hash, n) })
	started := time.Now()
	logging.Infof("storage migration starting hash=%s state=%s", t.Hash, m.State)
	root, err := os.OpenRoot(uc.engine.DownloadDir())
	if err != nil {
		release()
		return err
	}
	defer root.Close()
	// Reserve all legacy sources and the target namespace through copy, commit
	// and cleanup. Unrelated Add/Delete keep independent command handles.
	pathsForClaim, _, claimErr := uc.metadataStoragePaths(t)
	if claimErr != nil {
		release()
		return claimErr
	}
	claimPaths := []string{payload.Root(t.Hash)}
	for _, p := range pathsForClaim {
		claimPaths = append(claimPaths, p, p+".part")
	}
	if m.Manifest != "" {
		var saved migrationJournal
		if err := json.Unmarshal([]byte(m.Manifest), &saved); err != nil {
			release()
			return err
		}
		for _, e := range saved.Entries {
			claimPaths = append(claimPaths, e.Source, e.Target)
		}
	}
	ownershipRelease, claimErr := uc.reserveOwnership(ctx, root, t.Hash, claimPaths)
	if claimErr != nil {
		if errors.Is(claimErr, ErrStorageConflict) && ctx.Err() == nil {
			m.State = "blocked"
			m.Error = "Legacy storage ownership conflict; explicit retry required"
			_ = uc.repo.SaveStorageMappingForRecord(m, recordID)
		}
		release()
		return claimErr
	}
	defer ownershipRelease()
	var journal migrationJournal
	if m.Manifest != "" {
		err = json.Unmarshal([]byte(m.Manifest), &journal)
	} else {
		journal.Root = payload.Root(t.Hash)
		var paths []string
		var known bool
		paths, known, err = uc.metadataStoragePaths(t)
		if err == nil && !known {
			err = fmt.Errorf("original metainfo required for legacy migration")
		}
		if err == nil {
			logging.Infof("storage migration hash=%s stage=preflight files=%d", t.Hash, len(paths))
			err = uc.preflightMigration(ctx, root, t, paths, journal.Root, &journal)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			release()
			return ctx.Err()
		}
		m.State = "blocked"
		m.Error = "Legacy storage conflict; restore metainfo or resolve ownership before retry"
		_ = uc.repo.SaveStorageMapping(m)
		release()
		return err
	}
	if m.State != "ready" {
		mi, e := metainfo.LoadFromFile(filepath.Join(uc.engine.ConfigDir(), "metainfo", t.Hash+".torrent"))
		if e != nil {
			release()
			return e
		}
		claim := *m
		claim.Root = journal.Root
		claim.State = "moving"
		claim.Version = 0
		claim.Manifest = ""
		if err := uc.registerStorageAliases(&claim, mi); err != nil {
			release()
			return err
		}
		m.State = "moving"
		m.Error = ""
		if err = saveMigrationJournal(uc.repo, m, &journal); err != nil {
			release()
			return err
		}
	}
	release()
	defer func() {
		if retErr != nil && m.Manifest != "" {
			m.Error = "Storage migration interrupted; source preserved; retry scheduled"
			_ = uc.repo.SaveStorageMappingForRecord(m, recordID)
		}
	}()
	if m.State != "ready" {
		if err := uc.copyMigrationEntries(ctx, root, m, &journal); err != nil {
			return err
		}
		logging.Infof("storage migration hash=%s stage=verification files=%d duration=%s", t.Hash, len(journal.Entries), time.Since(started))
		mi, err := metainfo.LoadFromFile(filepath.Join(uc.engine.ConfigDir(), "metainfo", strings.ToLower(t.Hash)+".torrent"))
		if err != nil {
			return err
		}
		uc.storageStage(t.Hash, "verification", nil, nil)
		if err := engine.VerifyStorageProgress(ctx, uc.engine.DownloadDir(), t.Hash, mi, uc.storageIORead(t.Hash)); err != nil {
			return err
		}
		release, err = uc.lockRecords(ctx, t.Hash)
		if err != nil {
			return err
		}
		m.Version = payload.LayoutVersion
		m.Root = journal.Root
		m.State = "ready"
		m.Error = ""
		err = saveMigrationJournal(uc.repo, m, &journal)
		release()
		if err != nil {
			return err
		}
	}
	// Commit precedes source cleanup. The journal remains until cleanup succeeds.
	logging.Infof("storage migration hash=%s stage=cleanup files=%d duration=%s", t.Hash, len(journal.Entries), time.Since(started))
	lastProgress := time.Now()
	// A resumed cleanup may already lack some sources; do not present its
	// maximum possible re-read volume as a known total.
	uc.storageStage(t.Hash, "cleanup", nil, int64ptr(int64(len(journal.Entries))))
	for index, entry := range journal.Entries {
		uc.storageFile(t.Hash, entry.Source, nil)
		if err := ctx.Err(); err != nil {
			return err
		}
		// A committed journal may be resumed after a crash. Re-identify the
		// durable copy before discarding the only remaining legacy source.
		targetDigest, targetSize, err := digestPayload(ctx, root, entry.Target)
		if err != nil || targetDigest != entry.Digest || targetSize != entry.Size {
			return fmt.Errorf("committed migration target changed; source retained")
		}
		if _, err := root.Lstat(entry.Source); errors.Is(err, os.ErrNotExist) {
			uc.storageFileDone(t.Hash)
			continue
		}
		digest, _, err := digestPayload(ctx, root, entry.Source)
		if err != nil {
			return err
		}
		if digest != entry.Digest {
			return fmt.Errorf("legacy source changed after migration; retained")
		}
		if err := root.Remove(entry.Source); err != nil {
			return err
		}
		uc.storageFileDone(t.Hash)
		if time.Since(lastProgress) >= 30*time.Second {
			logging.Infof("storage migration hash=%s stage=cleanup files=%d/%d", t.Hash, index+1, len(journal.Entries))
			lastProgress = time.Now()
		}
	}
	release, err = uc.lockRecords(ctx, t.Hash)
	if err != nil {
		return err
	}
	defer release()
	i, err := uc.currentIntent(t.Hash)
	if err != nil {
		return err
	}
	if i.Version.RecordID != recordID {
		return ErrStorageConflict
	}
	final := *m
	final.Manifest = ""
	final.Error = ""
	mi, err := metainfo.LoadFromFile(filepath.Join(uc.engine.ConfigDir(), "metainfo", t.Hash+".torrent"))
	if err != nil {
		return err
	}
	if err := uc.registerStorageAliases(&final, mi); err != nil {
		return err
	}
	if err := uc.repo.SaveStorageMapping(&final); err != nil {
		return err
	}
	*m = final
	current, err = uc.repo.GetTorrent(t.Hash)
	if err != nil || current == nil {
		return err
	}
	if rec, err := uc.repo.Recovery(t.Hash); err != nil {
		return err
	} else if recoveryBlocks(rec) {
		return nil
	}
	err = uc.restoreMappedTorrentLocked(current)
	if err == nil {
		logging.Infof("storage migration finished hash=%s files=%d duration=%s", t.Hash, len(journal.Entries), time.Since(started))
	}
	return err
}

const migrationJournalBatchSize = 64

func (uc *TorrentUseCase) copyMigrationEntries(ctx context.Context, root *os.Root, m *repository.StorageMapping, journal *migrationJournal) error {
	logging.Infof("storage migration hash=%s stage=copy files=%d", m.Hash, len(journal.Entries))
	lastProgress := time.Now()
	for start := 0; start < len(journal.Entries); start += migrationJournalBatchSize {
		end := min(start+migrationJournalBatchSize, len(journal.Entries))
		batch := journal.Entries[start:end]
		var checksumBytes int64
		checksumKnown := true
		checksumFiles := int64(0)
		for _, entry := range batch {
			if entry.Digest == "" {
				checksumBytes += entry.Size
				checksumFiles++
				if entry.Size == 0 {
					checksumKnown = false
				} // old journals did not persist preflight sizes
			}
		}
		var checksumTotal *int64
		if checksumKnown {
			checksumTotal = int64ptr(checksumBytes)
		}
		uc.storageStage(m.Hash, "checksum", checksumTotal, int64ptr(checksumFiles))
		// Persist fingerprints BEFORE publishing any files in this batch. A crash
		// can lose Copied flags, but never ownership or the expected digest of a
		// published target. Restart always revalidates it, even if Copied is true.
		changed := false
		for i := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			entry := &batch[i]
			if entry.Digest == "" {
				var fileTotal *int64
				if entry.Size > 0 {
					fileTotal = int64ptr(entry.Size)
				}
				uc.storageFile(m.Hash, entry.Source, fileTotal)
				digest, size, err := digestPayload(ctx, root, entry.Source)
				if err != nil {
					return err
				}
				entry.Digest, entry.Size = digest, size
				changed = true
				uc.storageFileDone(m.Hash)
			}
		}
		if changed {
			if err := saveMigrationJournal(uc.repo, m, journal); err != nil {
				return err
			}
		}
		uc.storageStage(m.Hash, "copy", nil, int64ptr(int64(len(batch))))
		for i := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			entry := &batch[i]
			uc.storageFile(m.Hash, entry.Source, nil)
			if err := root.MkdirAll(filepath.Dir(entry.Target), 0700); err != nil {
				return err
			}
			if _, err := root.Lstat(entry.Target); errors.Is(err, os.ErrNotExist) {
				uc.storageFile(m.Hash, entry.Source, int64ptr(2*entry.Size))
				if err := uc.engine.CheckMigrationSpace(filepath.Join(uc.engine.DownloadDir(), filepath.Dir(entry.Target)), uint64(entry.Size)); err != nil {
					return err
				}
			} else if err == nil {
				uc.storageFile(m.Hash, entry.Target, int64ptr(entry.Size))
			}
			copyEntry := uc.migrationCopy
			if copyEntry == nil {
				copyEntry = copyMigrationEntry
			}
			if err := copyEntry(ctx, root, entry); err != nil {
				return err
			}
			entry.Copied = true
			uc.storageFileDone(m.Hash)
			if time.Since(lastProgress) >= 30*time.Second || start+i+1 == len(journal.Entries) {
				logging.Infof("storage migration hash=%s stage=copy files=%d/%d", m.Hash, start+i+1, len(journal.Entries))
				lastProgress = time.Now()
			}
		}
		if err := saveMigrationJournal(uc.repo, m, journal); err != nil {
			return err
		}
	}
	return nil
}

func (uc *TorrentUseCase) restoreMappedTorrentLocked(t *models.Torrent) error {
	if uc.engine.GetTorrent(t.Hash) != nil {
		return nil
	}
	i, err := uc.currentIntent(t.Hash)
	if err != nil {
		return err
	}
	blocks, err := uc.repo.ActivationBlocks(i.Version)
	if err != nil {
		return err
	}
	// Persisted operational state/reason are observations, not restore authority.
	t.State, t.Error = restoreActivation(i.Run, t.DownloadBlock, blocks)
	if i.Run == models.RunPaused || len(blocks) > 0 {
		if err := uc.restoreInactiveLocked(t); err != nil {
			return err
		}
		for _, b := range blocks {
			if b.Kind == models.BlockVerification {
				if err := uc.engine.RestoreVerificationBlock(t.Hash); err != nil {
					return err
				}
			}
		}
		expected := uc.engine.GetRawTorrent(t.Hash)
		if err := bindAppliedRuntime(uc.engine, uc.repo, i.Version); err != nil {
			return uc.failedIntentApply(expected, i.Version, "runtime_bind_failed", err)
		}
		return nil
	}
	if _, err := uc.restoreTorrentToEngine(t); err != nil {
		return err
	}
	expected := uc.engine.GetRawTorrent(t.Hash)
	if err := uc.engine.FinishRestore(t.Hash, t.State); err != nil {
		return uc.failedIntentApply(expected, i.Version, "runtime_apply_failed", err)
	}
	if err := bindAppliedRuntime(uc.engine, uc.repo, i.Version); err != nil {
		return uc.failedIntentApply(expected, i.Version, "runtime_bind_failed", err)
	}
	return nil
}

func (uc *TorrentUseCase) registerStorageAliases(m *repository.StorageMapping, mi *metainfo.MetaInfo) error {
	spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		return err
	}
	var aliases []string
	if spec.InfoHash != (metainfo.Hash{}) {
		aliases = append(aliases, spec.InfoHash.HexString())
	}
	if spec.InfoHashV2.Ok {
		v2 := spec.InfoHashV2.Value
		aliases = append(aliases, v2.ToShort().HexString())
	}
	for _, hash := range aliases {
		if hash == m.Hash {
			continue
		}
		old, err := uc.repo.StorageMapping(hash)
		if err != nil {
			return err
		}
		if old != nil && old.Root != m.Root {
			return fmt.Errorf("storage alias already has a different owner")
		}
		copy := *m
		copy.Hash = hash
		if err := uc.publishStorage(&copy, true, false, m.Hash); err != nil {
			return err
		}
	}
	return nil
}

func (uc *TorrentUseCase) preflightMigration(ctx context.Context, root *os.Root, t *models.Torrent, paths []string, targetRoot string, journal *migrationJournal) error {
	if _, err := root.Lstat(targetRoot); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("isolated target occupied")
	}
	var owned []string
	for _, p := range paths {
		owned = append(owned, p, p+".part")
	}
	if err := uc.validateOwnershipContext(ctx, root, t.Hash, append(append([]string{}, owned...), targetRoot), nil); err != nil {
		return err
	}
	for _, name := range owned {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := payload.Validate(root, name); err != nil {
			return err
		}
		st, err := root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if pathsOverlap(name, targetRoot) {
			return fmt.Errorf("legacy source overlaps new namespace")
		}
		journal.Entries = append(journal.Entries, migrationEntry{Source: name, Target: filepath.Join(targetRoot, name), Size: st.Size()})
	}
	return validateMigrationTemporaryPaths(ctx, journal.Entries)
}

func validateMigrationTemporaryPaths(ctx context.Context, entries []migrationEntry) error {
	// Index ancestors as well as exact targets, so prefix conflicts need no
	// quadratic scan of the catalog (file, file.tsh-migrate/child).
	targets := make(map[string]bool, len(entries))
	for _, e := range entries {
		targets[filepath.Clean(e.Target)] = true
	}
	sorted := make([]string, 0, len(targets))
	for target := range targets {
		sorted = append(sorted, target)
	}
	sort.Strings(sorted)
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		temp := filepath.Clean(e.Target + ".tsh-migrate")
		for parent := temp; parent != "."; parent = filepath.Dir(parent) {
			if targets[parent] {
				return fmt.Errorf("migration temporary path conflict")
			}
		}
		prefix := temp + string(os.PathSeparator)
		index := sort.SearchStrings(sorted, prefix)
		if index < len(sorted) && strings.HasPrefix(sorted[index], prefix) {
			return fmt.Errorf("migration temporary path conflict")
		}
	}
	return nil
}

func saveMigrationJournal(repo *repository.TorrentRepo, m *repository.StorageMapping, j *migrationJournal) error {
	encoded, err := json.Marshal(j)
	if err != nil {
		return err
	}
	m.Manifest = string(encoded)
	return repo.SaveStorageMapping(m)
}

type migrationReader struct {
	ctx context.Context
	io.Reader
}
type migrationProgressKey struct{}

func (r migrationReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.Reader.Read(p)
	if observe, ok := r.ctx.Value(migrationProgressKey{}).(func(int64)); ok && n > 0 {
		observe(int64(n))
	}
	return n, err
}

func digestPayload(ctx context.Context, root *os.Root, path string) (string, int64, error) {
	f, err := payload.Open(root, path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.CopyBuffer(h, migrationReader{ctx, f}, make([]byte, 128<<10))
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func copyMigrationEntry(ctx context.Context, root *os.Root, e *migrationEntry) error {
	temp := e.Target + ".tsh-migrate"
	if _, err := root.Lstat(e.Target); err == nil {
		// Recover a crash between atomic no-replace publication and unlinking
		// the journal-owned temporary hardlink.
		a, ae := root.Lstat(e.Target)
		b, be := root.Lstat(temp)
		if ae == nil && be == nil && a.Mode().IsRegular() && b.Mode().IsRegular() && os.SameFile(a, b) {
			if err := root.Remove(temp); err != nil {
				return err
			}
		}
		digest, size, err := digestPayload(ctx, root, e.Target)
		if err != nil || digest != e.Digest || size != e.Size {
			return fmt.Errorf("migration target conflict")
		}
		return syncMigrationDirectories(root, e.Target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := payload.Validate(root, temp); err != nil {
		return err
	}
	if err := root.MkdirAll(filepath.Dir(temp), 0700); err != nil {
		return err
	}
	// The journal already owns this temporary name. Restart can discard a short
	// prior copy, but never an unrelated final target.
	if err := root.Remove(temp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	in, err := payload.Open(root, e.Source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.CopyBuffer(out, migrationReader{ctx, in}, make([]byte, 128<<10))
	syncErr := out.Sync()
	closeErr := out.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		return err
	}
	digest, size, err := digestPayload(ctx, root, temp)
	if err != nil || digest != e.Digest || size != e.Size {
		return fmt.Errorf("source changed during migration copy")
	}
	if err := root.Link(temp, e.Target); err != nil {
		return err
	}
	if err := root.Remove(temp); err != nil {
		return err
	}
	return syncMigrationDirectories(root, e.Target)
}

func syncMigrationDirectories(root *os.Root, target string) error {
	// Persist directory entries before a SQLite commit can authorize removal of
	// the source. This includes newly created ancestors of the isolated root.
	for dir := filepath.Dir(target); ; dir = filepath.Dir(dir) {
		if err := storage.SyncRootDirectory(root, dir); err != nil {
			return err
		}
		if dir == "." {
			break
		}
	}
	return nil
}
