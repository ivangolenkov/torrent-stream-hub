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

func (uc *TorrentUseCase) initializeStorageLocked() error {
	all, err := uc.repo.GetAllTorrents()
	if err != nil {
		return err
	}
	for _, t := range all {
		m, err := uc.repo.StorageMapping(t.Hash)
		if err != nil {
			return err
		}
		if m == nil {
			if err := uc.repo.SaveStorageMapping(&repository.StorageMapping{Hash: t.Hash, State: "legacy"}); err != nil {
				return err
			}
		}
	}
	if !uc.storageWorkerStarted {
		uc.storageWorkerStarted = true
		uc.engine.RunStorageTask(uc.runStorageMigrations)
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
			if err := uc.repo.SaveStorageMapping(&copy); err != nil {
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
		if err := uc.repo.SaveStorageMapping(m); err != nil {
			return err
		}
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
			if err := uc.repo.SaveStorageMapping(&copy); err != nil {
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
		uc.repo.Lifecycle.Lock()
		all, err := uc.repo.GetAllTorrents()
		uc.repo.Lifecycle.Unlock()
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

// Migration does not hold Lifecycle during copying or hashing. Its durable
// moving state rejects admission, re-add, Resume and deletion of this instance.
func (uc *TorrentUseCase) migrateStorage(ctx context.Context, t *models.Torrent) (retErr error) {
	uc.repo.Lifecycle.Lock()
	m, err := uc.repo.StorageMapping(t.Hash)
	if err != nil || m == nil {
		uc.repo.Lifecycle.Unlock()
		return err
	}
	_, pending, err := uc.repo.CleanupPaths(t.Hash)
	if err != nil || pending {
		uc.repo.Lifecycle.Unlock()
		return err
	}
	if m.State == "ready" && m.Manifest == "" {
		uc.repo.Lifecycle.Unlock()
		return nil
	}
	root, err := os.OpenRoot(uc.engine.DownloadDir())
	if err != nil {
		uc.repo.Lifecycle.Unlock()
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
			err = uc.preflightMigration(root, t, paths, journal.Root, &journal)
		}
	}
	if err != nil {
		m.State = "blocked"
		m.Error = "Legacy storage conflict; restore metainfo or resolve ownership before retry"
		_ = uc.repo.SaveStorageMapping(m)
		uc.repo.Lifecycle.Unlock()
		return err
	}
	if m.State != "ready" {
		mi, e := metainfo.LoadFromFile(filepath.Join(uc.engine.ConfigDir(), "metainfo", t.Hash+".torrent"))
		if e != nil {
			uc.repo.Lifecycle.Unlock()
			return e
		}
		claim := *m
		claim.Root = journal.Root
		claim.State = "moving"
		claim.Version = 0
		claim.Manifest = ""
		if err := uc.registerStorageAliases(&claim, mi); err != nil {
			uc.repo.Lifecycle.Unlock()
			return err
		}
		m.State = "moving"
		m.Error = ""
		if err = saveMigrationJournal(uc.repo, m, &journal); err != nil {
			uc.repo.Lifecycle.Unlock()
			return err
		}
	}
	uc.repo.Lifecycle.Unlock()
	defer func() {
		if retErr != nil {
			m.Error = "Storage migration interrupted; source preserved; retry scheduled"
			_ = uc.repo.SaveStorageMapping(m)
		}
	}()
	if m.State != "ready" {
		for i := range journal.Entries {
			entry := &journal.Entries[i]
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.Digest == "" {
				digest, size, err := digestPayload(ctx, root, entry.Source)
				if err != nil {
					return err
				}
				entry.Digest, entry.Size = digest, size
				if err := saveMigrationJournal(uc.repo, m, &journal); err != nil {
					return err
				}
			}
			if err := root.MkdirAll(filepath.Dir(entry.Target), 0700); err != nil {
				return err
			}
			if _, err := root.Lstat(entry.Target); errors.Is(err, os.ErrNotExist) {
				if err := uc.engine.CheckMigrationSpace(filepath.Join(uc.engine.DownloadDir(), filepath.Dir(entry.Target)), uint64(entry.Size)); err != nil {
					return err
				}
			}
			if err := copyMigrationEntry(ctx, root, entry); err != nil {
				return err
			}
			entry.Copied = true
			if err := saveMigrationJournal(uc.repo, m, &journal); err != nil {
				return err
			}
		}
		mi, err := metainfo.LoadFromFile(filepath.Join(uc.engine.ConfigDir(), "metainfo", strings.ToLower(t.Hash)+".torrent"))
		if err != nil {
			return err
		}
		if err := engine.VerifyStorage(ctx, uc.engine.DownloadDir(), t.Hash, mi); err != nil {
			return err
		}
		uc.repo.Lifecycle.Lock()
		m.Version = payload.LayoutVersion
		m.Root = journal.Root
		m.State = "ready"
		m.Error = ""
		err = saveMigrationJournal(uc.repo, m, &journal)
		uc.repo.Lifecycle.Unlock()
		if err != nil {
			return err
		}
	}
	// Commit precedes source cleanup. The journal remains until cleanup succeeds.
	for _, entry := range journal.Entries {
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
	uc.repo.Lifecycle.Lock()
	defer uc.repo.Lifecycle.Unlock()
	current, err := uc.repo.GetTorrent(t.Hash)
	if err != nil || current == nil {
		return err
	}
	return uc.restoreMappedTorrentLocked(current)
}

func (uc *TorrentUseCase) restoreMappedTorrentLocked(t *models.Torrent) error {
	if uc.engine.GetTorrent(t.Hash) != nil {
		return nil
	}
	if t.State == models.StatePaused || t.State == models.StateError || t.State == models.StateMissingFiles {
		return uc.restoreInactiveLocked(t)
	}
	if _, err := uc.restoreTorrentToEngine(t); err != nil {
		return err
	}
	return uc.engine.FinishRestore(t.Hash, t.State)
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
		if err := uc.repo.SaveStorageMapping(&copy); err != nil {
			return err
		}
	}
	return nil
}

func (uc *TorrentUseCase) preflightMigration(root *os.Root, t *models.Torrent, paths []string, targetRoot string, journal *migrationJournal) error {
	if _, err := root.Lstat(targetRoot); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("isolated target occupied")
	}
	all, err := uc.repo.GetAllTorrents()
	if err != nil {
		return err
	}
	var owned []string
	for _, p := range paths {
		owned = append(owned, p, p+".part")
	}
	if err := uc.validateOwnership(root, t.Hash, append(append([]string{}, owned...), targetRoot), all); err != nil {
		return err
	}
	for _, name := range owned {
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
	// Temporary copy names cannot collide with another metadata file.
	for i, a := range journal.Entries {
		for k, b := range journal.Entries {
			if i != k && pathsOverlap(a.Target+".tsh-migrate", b.Target) {
				return fmt.Errorf("migration temporary path conflict")
			}
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
