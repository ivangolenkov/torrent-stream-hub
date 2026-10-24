package usecase

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"torrent-stream-hub/internal/completion"
	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

func TestStorageMigrationWithSQLiteCompletion(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DownloadDir: dir, DBPath: filepath.Join(dir, "hub.db"),
		BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true, BTDisableUTP: true, BTRetrackersMode: "off"}
	db, err := repository.NewSQLiteDB(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pc, err := completion.New(db.DB())
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(cfg, engine.WithPieceCompletion(pc))
	if err != nil {
		pc.Close()
		t.Fatal(err)
	}
	defer eng.Close()
	uc := NewTorrentUseCase(eng, repository.NewTorrentRepo(db))
	data := []byte("already downloaded")
	tor, _ := legacyMigrationFixture(t, uc, migrationMetainfo(t, "existing", data, false, "v1"), data, false)
	if err := uc.migrateStorage(context.Background(), tor); err != nil {
		t.Fatal(err)
	}
	waitMigrationReady(t, uc, tor.Hash)
	if _, err := os.Stat(filepath.Join(dir, payload.Root(tor.Hash), "existing.part")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verification created a second partial file: %v", err)
	}
}

func TestStorageMigrationBatchedJournalRestart(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	const count = 130 // More than two journal batches, including a partial batch.
	info := metainfo.Info{Name: "many", PieceLength: 16384}
	for i := 0; i < count; i++ {
		info.Files = append(info.Files, metainfo.FileInfo{Length: 1, Path: []string{fmt.Sprintf("%04d", i)}})
	}
	h := sha1.Sum(bytes.Repeat([]byte("x"), count))
	info.Pieces = h[:]
	encoded, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	tor, _ := legacyMigrationFixture(t, uc, &metainfo.MetaInfo{InfoBytes: encoded}, []byte("x"), false)
	tor.Files = nil
	for i, f := range info.Files {
		name := filepath.Join(uc.engine.DownloadDir(), info.Name, f.Path[0])
		if err := os.WriteFile(name, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		tor.Files = append(tor.Files, &models.File{Index: i, Path: f.Path[0], Size: 1})
	}
	if err := repo.SaveTorrent(tor); err != nil {
		t.Fatal(err)
	}
	// Count actual SQLite journal updates, including updates made by recovery.
	db, err := repository.NewSQLiteDB(filepath.Join(uc.engine.DownloadDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB().Exec(`CREATE TABLE journal_writes(n INTEGER); INSERT INTO journal_writes VALUES(0);
		CREATE TRIGGER count_journal AFTER UPDATE ON torrent_storage BEGIN UPDATE journal_writes SET n=n+1; END`); err != nil {
		t.Fatal(err)
	}
	copies := 0
	interrupted := errors.New("interrupted after publication, before batch checkpoint")
	uc.migrationCopy = func(ctx context.Context, root *os.Root, entry *migrationEntry) error {
		if err := copyMigrationEntry(ctx, root, entry); err != nil {
			return err
		}
		copies++
		if copies == 65 {
			return interrupted
		}
		return nil
	}
	if err := uc.migrateStorage(context.Background(), tor); !errors.Is(err, interrupted) {
		t.Fatalf("expected interruption: %v", err)
	}
	m, err := repo.StorageMapping(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	var journal migrationJournal
	if err := json.Unmarshal([]byte(m.Manifest), &journal); err != nil {
		t.Fatal(err)
	}
	entry := journal.Entries[64]
	if entry.Digest == "" || entry.Copied || m.State != "moving" {
		t.Fatalf("published copy lost its durable fingerprint: %+v", entry)
	}
	for _, f := range info.Files {
		if _, err := os.Stat(filepath.Join(uc.engine.DownloadDir(), info.Name, f.Path[0])); err != nil {
			t.Fatalf("source lost before commit: %v", err)
		}
	}
	// Restart must validate a published file even without a durable Copied flag.
	uc.migrationCopy = nil
	target := filepath.Join(uc.engine.DownloadDir(), entry.Target)
	if err := os.WriteFile(target, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := uc.migrateStorage(context.Background(), tor); err == nil {
		t.Fatal("changed copy accepted after restart")
	}
	if err := os.WriteFile(target, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`UPDATE journal_writes SET n=0`); err != nil {
		t.Fatal(err)
	}
	if err := uc.migrateStorage(context.Background(), tor); err != nil {
		t.Fatal(err)
	}
	var writes int
	if err := db.DB().QueryRow(`SELECT n FROM journal_writes`).Scan(&writes); err != nil {
		t.Fatal(err)
	}
	if writes > 2*((count+63)/64)+3 {
		t.Fatalf("journal writes scale per file: %d for %d files", writes, count)
	}
	t.Logf("resumed %d files with %d journal updates", count, writes)
	m, err = repo.StorageMapping(tor.Hash)
	if err != nil || m.State != "ready" || m.Manifest != "" {
		t.Fatalf("migration not completed: %+v %v", m, err)
	}
	for _, f := range info.Files {
		if _, err := os.Stat(filepath.Join(uc.engine.DownloadDir(), info.Name, f.Path[0])); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("source remains after commit: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(uc.engine.DownloadDir(), payload.Root(tor.Hash), info.Name, f.Path[0]))
		if err != nil || string(data) != "x" {
			t.Fatalf("copied file changed: %q %v", data, err)
		}
	}
}

func TestStorageMigrationFingerprintCommitFailureKeepsSource(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	data := []byte("original")
	tor, source := legacyMigrationFixture(t, uc, migrationMetainfo(t, "commit", data, false, "v1"), data, false)
	db, err := repository.NewSQLiteDB(filepath.Join(uc.engine.DownloadDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB().Exec(`CREATE TRIGGER fail_fingerprint BEFORE UPDATE ON torrent_storage
		WHEN NEW.state='moving' AND instr(NEW.manifest, '"Digest":""')=0
		BEGIN SELECT RAISE(ABORT,'fingerprint commit failed'); END`); err != nil {
		t.Fatal(err)
	}
	if err := uc.migrateStorage(context.Background(), tor); err == nil {
		t.Fatal("fingerprint persistence failure ignored")
	}
	got, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("source lost: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(uc.engine.DownloadDir(), payload.Root(tor.Hash))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("copy published before fingerprint commit: %v", err)
	}
	m, err := repo.StorageMapping(tor.Hash)
	if err != nil || m.State != "moving" || m.Manifest == "" {
		t.Fatalf("ownership journal lost: %+v %v", m, err)
	}
}

func TestStorageOwnershipRechecksAliasesAfterFilesystemChange(t *testing.T) {
	uc, _, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	data := []byte("file")
	one, source := legacyMigrationFixture(t, uc, migrationMetainfo(t, "one", data, false, "v1"), data, false)
	two, other := legacyMigrationFixture(t, uc, migrationMetainfo(t, "two", data, false, "v1"), data, false)
	root, err := os.OpenRoot(uc.engine.DownloadDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	all := []*models.Torrent{one, two}
	if err := uc.validateOwnership(root, one.Hash, []string{"one"}, all); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, other); err != nil {
		t.Skip(err)
	}
	if err := uc.validateOwnership(root, one.Hash, []string{"one"}, all); !errors.Is(err, ErrStorageConflict) {
		t.Fatalf("new alias was hidden by a previous check: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := uc.validateOwnershipContext(ctx, root, one.Hash, []string{"one"}, all); !errors.Is(err, context.Canceled) {
		t.Fatalf("ownership check ignored cancellation: %v", err)
	}
}

func TestMigrationTemporaryPathConflicts(t *testing.T) {
	for _, tc := range []struct {
		other    string
		conflict bool
	}{
		{"file.tsh-migrate", true},
		{"file.tsh-migrate/child", true},
		{"file.tsh-migrate-suffix", false},
		{"file/child", true},
		{"unrelated", false},
	} {
		t.Run(tc.other, func(t *testing.T) {
			entries := []migrationEntry{{Target: "root/file"}, {Target: filepath.Join("root", tc.other)}}
			err := validateMigrationTemporaryPaths(context.Background(), entries)
			if (err != nil) != tc.conflict {
				t.Fatalf("conflict=%t err=%v", tc.conflict, err)
			}
		})
	}
}

func TestStorageStatCacheBoundsIOAndDoesNotSurviveChecks(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "file")
	calls := 0
	probe := func(path string) (os.FileInfo, error) { calls++; return os.Stat(path) }
	stat := cachedStorageStat(probe)
	for i := 0; i < 4000; i++ {
		if _, err := stat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("repeated filesystem probes: %d", calls)
	}
	if err := os.WriteFile(name, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	stat = cachedStorageStat(probe)
	if _, err := stat(name); err != nil {
		t.Fatalf("new check reused an old observation: %v", err)
	}
}

func BenchmarkMigrationTemporaryPaths(b *testing.B) {
	entries := make([]migrationEntry, 4000)
	for i := range entries {
		entries[i].Target = fmt.Sprintf("payload/hash/directory/file-%04d", i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := validateMigrationTemporaryPaths(context.Background(), entries); err != nil {
			b.Fatal(err)
		}
	}
}
