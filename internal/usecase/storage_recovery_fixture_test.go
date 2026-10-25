package usecase

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"
)

// Mutate after the coherent snapshot, before filesystem/metainfo validation.
func TestStorageOwnershipDeletedRecordRetries(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	data := []byte("payload")
	one, source := legacyMigrationFixture(t, uc, migrationMetainfo(t, "one", data, false, "v1"), data, false)
	two, _ := legacyMigrationFixture(t, uc, migrationMetainfo(t, "two", data, false, "v1"), data, false)
	listed, deleted := make(chan struct{}), make(chan error, 1)
	go func() {
		<-listed
		deleted <- repo.DeleteTorrent(two.Hash)
	}()
	all, err := repo.GetAllTorrents()
	if err != nil {
		t.Fatal(err)
	}
	checks := 0
	uc.beforeOwnershipValidate = func() {
		checks++
		if checks == 1 {
			close(listed)
			if err := <-deleted; err != nil {
				t.Fatal(err)
			}
		}
	}
	root, err := os.OpenRoot(uc.engine.DownloadDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	err = uc.validateOwnershipContext(context.Background(), root, one.Hash, []string{"one"}, all)
	if err != nil || checks < 2 {
		t.Fatalf("catalog mutation did not retry safely: checks=%d error=%v", checks, err)
	}
	uc.beforeOwnershipValidate = nil
	// A new catalog passes without changing payload or restoring any metainfo.
	current, err := repo.GetAllTorrents()
	if err != nil {
		t.Fatal(err)
	}
	if err := uc.validateOwnership(root, one.Hash, []string{"one"}, current); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(source); err != nil || string(got) != string(data) {
		t.Fatalf("source changed: %q %v", got, err)
	}
}

func TestStorageRecoveryReaddedOwnerFixture(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	data := []byte("payload")
	one, _ := legacyMigrationFixture(t, uc, migrationMetainfo(t, "one", data, false, "v1"), data, false)
	two, _ := legacyMigrationFixture(t, uc, migrationMetainfo(t, "two", data, false, "v1"), data, false)
	all, err := repo.GetAllTorrents()
	if err != nil {
		t.Fatal(err)
	}
	before, err := repo.GetTorrent(two.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteTorrent(two.Hash); err != nil {
		t.Fatal(err)
	}
	// Re-add of the same hash has a new record identity and can acquire a new
	// overlapping mapping. A stale catalog entry is not grounds to ignore it.
	if err := repo.SaveTorrentWithPolicy(two, true); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveStorageMapping(&repository.StorageMapping{Hash: two.Hash, Version: payload.LayoutVersion, Root: "one", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	after, err := repo.GetTorrent(two.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if before.Control == nil || after.Control == nil || before.Control.RecordID == after.Control.RecordID {
		t.Fatal("fixture did not replace record identity")
	}
	root, err := os.OpenRoot(uc.engine.DownloadDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := uc.validateOwnership(root, one.Hash, []string{"one"}, all); !errors.Is(err, ErrStorageConflict) {
		t.Fatalf("re-added owner must not be ignored: %v", err)
	}
}

// Durable fixtures for the deletion lifecycle: the old manifest intentionally
// has no unlink counters, so a reopened record cannot prove data preservation.
func TestStorageRecoveryPendingDeletionFixtures(t *testing.T) {
	for _, partial := range []bool{false, true} {
		name := "before-unlink"
		if partial {
			name = "after-unlink"
		}
		t.Run(name, func(t *testing.T) {
			uc, repo, cleanup := setupTorrentUseCase(t)
			defer cleanup()
			data := []byte("remaining")
			tor, source := legacyMigrationFixture(t, uc, migrationMetainfo(t, name, data, false, "v1"), data, false)
			if err := os.WriteFile(source+".part", data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := repo.BeginCleanup(tor.Hash, []string{name}); err != nil {
				t.Fatal(err)
			}
			if err := repo.SaveCleanupPaths(tor.Hash, []string{name}); err != nil {
				t.Fatal(err)
			}
			if partial {
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
			}
			// A separate pool reads the durable intent and old manifest, not memory.
			db, err := repository.NewSQLiteDB(filepath.Join(uc.engine.DownloadDir(), "hub.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			reopened := repository.NewTorrentRepo(db)
			got, err := reopened.GetTorrent(tor.Hash)
			if err != nil || got == nil || got.DeletionMode != "payload" {
				t.Fatalf("lost intent: %+v %v", got, err)
			}
			paths, pending, err := reopened.CleanupPaths(tor.Hash)
			if err != nil || !pending || len(paths) != 1 || paths[0] != name {
				t.Fatalf("lost manifest: %v %v %v", paths, pending, err)
			}
			if got, err := os.ReadFile(source + ".part"); err != nil || string(got) != string(data) {
				t.Fatalf("remaining data changed: %q %v", got, err)
			}
		})
	}
}

// A large logical reader with fixed memory cost and one externally controlled
// blocked syscall. It deliberately does not observe context while blocked:
// cancellation of a waiter cannot claim that its resource is released.
type recoveryLogicalReader struct {
	remaining int64
	entered   chan struct{}
	release   chan struct{}
	blocked   bool
}

type recoveryBlockedWriter struct {
	entered chan struct{}
	release chan struct{}
	bytes   int64
}

func (w *recoveryBlockedWriter) Write(p []byte) (int, error) {
	close(w.entered)
	<-w.release
	w.bytes += int64(len(p))
	return len(p), nil
}

func TestStorageRecoverySlowWriterFixture(t *testing.T) {
	w := &recoveryBlockedWriter{entered: make(chan struct{}), release: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		_, err := io.CopyBuffer(w, io.LimitReader(strings.NewReader("data"), 4), make([]byte, 128<<10))
		result <- err
	}()
	<-w.entered
	select {
	case err := <-result:
		t.Fatalf("blocked writer returned: %v", err)
	default:
	}
	close(w.release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if w.bytes != 4 {
		t.Fatalf("wrong byte count: %d", w.bytes)
	}
}

func (r *recoveryLogicalReader) Read(p []byte) (int, error) {
	if !r.blocked {
		r.blocked = true
		close(r.entered)
		<-r.release
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), r.remaining))
	clear(p[:n])
	r.remaining -= int64(n)
	return n, nil
}

func TestStorageRecoverySlowIOFixture(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &recoveryLogicalReader{remaining: 8 << 30, entered: make(chan struct{}), release: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		_, err := io.CopyBuffer(io.Discard, migrationReader{ctx, r}, make([]byte, 128<<10))
		result <- err
	}()
	<-r.entered
	cancel()
	select {
	case err := <-result:
		t.Fatalf("claimed completion before syscall returned: %v", err)
	default:
	}
	close(r.release)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	if r.remaining >= 8<<30 || r.remaining < (8<<30)-(128<<10) {
		t.Fatalf("unbounded read: %d remaining", r.remaining)
	}
}
