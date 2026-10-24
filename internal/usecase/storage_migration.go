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
			if err := uc.repo.SaveStorageAlias(&copy); err != nil {
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
		all, err := uc.repo.GetAllTorrents()
		if err != nil {
			return err
		}
		if err := uc.validateOwnership(root, hash, []string{rel}, all); err != nil {
			return err
		}
		m = &repository.StorageMapping{Hash: hash, Version: payload.LayoutVersion, Root: rel, State: "ready"}
		if err := uc.repo.EnsureStorageMapping(m); err != nil {
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
			if err := uc.repo.SaveStorageAlias(&copy); err != nil {
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
			for _, t := range all {
				if ctx.Err() != nil {
					return
				}
				if err := uc.migrateStorage(ctx, t); err != nil && !errors.Is(err, context.Canceled) {
					logging.Warnf("storage migration hash=%s: %v", t.Hash, err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Migration releases command handles during copying/hashing. Its work ticket and durable
// moving state rejects admission, re-add, Resume and deletion of this instance.
func (uc *TorrentUseCase) migrateStorage(ctx context.Context, t *models.Torrent) (retErr error) {
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
	if m.State == "ready" && m.Manifest == "" {
		release()
		return nil
	}
	started := time.Now()
	logging.Infof("storage migration starting hash=%s state=%s", t.Hash, m.State)
	root, err := os.OpenRoot(uc.engine.DownloadDir())
	if err != nil {
		release()
		return err
	}
	defer root.Close()
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
			_ = uc.repo.SaveStorageMapping(m)
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
		if err := engine.VerifyStorage(ctx, uc.engine.DownloadDir(), t.Hash, mi); err != nil {
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
	for index, entry := range journal.Entries {
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
				digest, size, err := digestPayload(ctx, root, entry.Source)
				if err != nil {
					return err
				}
				entry.Digest, entry.Size = digest, size
				changed = true
			}
		}
		if changed {
			if err := saveMigrationJournal(uc.repo, m, journal); err != nil {
				return err
			}
		}
		for i := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			entry := &batch[i]
			if err := root.MkdirAll(filepath.Dir(entry.Target), 0700); err != nil {
				return err
			}
			if _, err := root.Lstat(entry.Target); errors.Is(err, os.ErrNotExist) {
				if err := uc.engine.CheckMigrationSpace(filepath.Join(uc.engine.DownloadDir(), filepath.Dir(entry.Target)), uint64(entry.Size)); err != nil {
					return err
				}
			}
			copyEntry := uc.migrationCopy
			if copyEntry == nil {
				copyEntry = copyMigrationEntry
			}
			if err := copyEntry(ctx, root, entry); err != nil {
				return err
			}
			entry.Copied = true
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
		if err := uc.repo.SaveStorageAlias(&copy); err != nil {
			return err
		}
	}
	return nil
}

func (uc *TorrentUseCase) preflightMigration(ctx context.Context, root *os.Root, t *models.Torrent, paths []string, targetRoot string, journal *migrationJournal) error {
	if _, err := root.Lstat(targetRoot); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("isolated target occupied")
	}
	all, err := uc.repo.GetAllTorrentsContext(ctx)
	if err != nil {
		return err
	}
	var owned []string
	for _, p := range paths {
		owned = append(owned, p, p+".part")
	}
	if err := uc.validateOwnershipContext(ctx, root, t.Hash, append(append([]string{}, owned...), targetRoot), all); err != nil {
		return err
	}
	for _, name := range owned {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := payload.Validate(root, name); err != nil {
			return err
		}
		if _, err := root.Lstat(name); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if pathsOverlap(name, targetRoot) {
			return fmt.Errorf("legacy source overlaps new namespace")
		}
		journal.Entries = append(journal.Entries, migrationEntry{Source: name, Target: filepath.Join(targetRoot, name)})
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

func (r migrationReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
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
		f, err := root.Open(dir)
		if err != nil {
			return err
		}
		err = f.Sync()
		closeErr := f.Close()
		if err := errors.Join(err, closeErr); err != nil {
			return err
		}
		if dir == "." {
			break
		}
	}
	return nil
}
