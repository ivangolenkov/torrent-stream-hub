package usecase

import (
	"bytes"
	"context"
	"errors"
	"github.com/anacrolix/torrent/storage"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"
)

func moveFixture(t *testing.T, uc *TorrentUseCase, legacy bool, data []byte) (*models.Torrent, *repository.StorageMapping) {
	t.Helper()
	mi := migrationMetainfo(t, "Original", data, true, "v1")
	var input bytes.Buffer
	if err := mi.Write(&input); err != nil {
		t.Fatal(err)
	}
	tor, err := uc.AddTorrentFile(&input)
	if err != nil {
		t.Fatal(err)
	}
	if err := uc.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	if err := uc.engine.Delete(tor.Hash); err != nil {
		t.Fatal(err)
	}
	m, err := uc.repo.StorageMapping(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if legacy {
		m.Root = payload.Root(tor.Hash)
		m.Version = 1
		m.Prefix = ""
		if err := uc.repo.SaveStorageMapping(m); err != nil {
			t.Fatal(err)
		}
		m, _ = uc.repo.StorageMapping(tor.Hash)
	}
	info, _ := mi.UnmarshalInfo()
	for _, file := range info.UpvertedFiles() {
		p, err := m.Layout().File(&info, &file)
		if err != nil {
			t.Fatal(err)
		}
		p = filepath.Join(uc.engine.DownloadDir(), p)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if ready, err := uc.inspectInventory(context.Background(), tor.Hash, false); err != nil || !ready {
		t.Fatal("fixture inventory", ready, err)
	}
	tor, err = uc.repo.GetTorrent(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	return tor, m
}

func TestStorageMoveRestartBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, stage string
		legacy      bool
		occurrence  int
	}{
		{"before-rename", "location_publishing", false, 1},
		{"after-rename", "location_namespace_published", false, 1},
		{"after-mapping", "location_mapping_committed", false, 1},
		{"staging-identity", "location_copying", true, 2},
		{"copy-identities", "location_copying", true, 3},
		{"copied-file", "location_file_copied", true, 1},
		{"published-copy", "location_namespace_published", true, 1},
		{"cleanup-unlink", "location_source_removed", true, 1},
		{"cleanup-quarantine", "location_source_quarantined", true, 1},
		{"cleanup-directory", "location_cleanup_directory_removed", true, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			uc, _, closeFirst := setupTorrentUseCase(t)
			closed := false
			defer func() {
				if !closed {
					closeFirst()
				}
			}()
			events := make(chan string, 128)
			reached := make(chan struct{})
			seen := 0
			uc.recoveryStep = func(stage string) error {
				if stage == test.stage {
					seen++
					if seen == test.occurrence {
						close(reached)
						<-uc.engine.Context().Done()
						return context.Canceled
					}
				}
				select {
				case events <- stage:
				default:
				}
				return nil
			}
			data := bytes.Repeat([]byte("restart"), 333)
			tor, m := moveFixture(t, uc, test.legacy, data)
			dir := uc.engine.DownloadDir()
			cmd := planMoveFixture(t, uc, tor, m, events)
			if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
				t.Fatal(err)
			}
			select {
			case <-reached:
			case <-time.After(10 * time.Second):
				t.Fatal("boundary not reached", test.stage)
			}
			closeFirst()
			closed = true
			db, err := repository.NewSQLiteDB(filepath.Join(dir, "hub.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			eng, err := engine.New(&config.Config{DBPath: filepath.Join(dir, "hub.db"), DownloadDir: dir, BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true, BTDisableUTP: true, BTRetrackersMode: "off", MaxActiveDownloads: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer eng.Close()
			repo := repository.NewTorrentRepo(db)
			restored := NewTorrentUseCase(eng, repo)
			timeout := time.NewTimer(10 * time.Second)
			defer timeout.Stop()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
		wait:
			for {
				rec, err := repo.Recovery(tor.Hash)
				if err != nil {
					t.Fatal(err)
				}
				if rec != nil {
					if rec.Error != "" {
						t.Fatalf("restart failed: %+v", rec)
					}
					if rec.Stage == "completed" {
						break wait
					}
				}
				select {
				case <-tick.C:
				case <-timeout.C:
					rec, _ := repo.Recovery(tor.Hash)
					t.Fatalf("restart stalled: %+v", rec)
				}
			}
			after, _ := repo.StorageMapping(tor.Hash)
			actual, err := os.ReadFile(filepath.Join(dir, after.Root, "video.bin"))
			if err != nil || !bytes.Equal(actual, data) {
				t.Fatal("restart lost payload", err)
			}
			if after.Revision != m.Revision+1 {
				t.Fatal("mapping applied twice", after.Revision)
			}
			if _, err := restored.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
				t.Fatal("old apply not idempotent", err)
			}
		})
	}
}

func locationEvents(uc *TorrentUseCase) chan string {
	events := make(chan string, 128)
	uc.recoveryStep = func(stage string) error {
		select {
		case events <- stage:
		default:
		}
		return nil
	}
	return events
}
func waitLocationEvent(t *testing.T, uc *TorrentUseCase, hash string, events <-chan string, want string) {
	t.Helper()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case stage := <-events:
			if stage == want {
				return
			}
			if stage == "location_failed" {
				r, _ := uc.repo.Recovery(hash)
				t.Fatalf("operation failed: %+v", r)
			}
		case <-timeout.C:
			r, _ := uc.repo.Recovery(hash)
			t.Fatalf("waiting for %s: %+v", want, r)
		}
	}
}
func planMoveFixture(t *testing.T, uc *TorrentUseCase, tor *models.Torrent, m *repository.StorageMapping, events <-chan string) models.StorageCommand {
	t.Helper()
	cmd := models.StorageCommand{Action: "plan_move", RecordID: tor.Control.RecordID, StorageID: m.StorageID, MappingRevision: m.Revision, FolderName: "Moved"}
	if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
		t.Fatal(err)
	}
	waitLocationEvent(t, uc, tor.Hash, events, "location_planned")
	rec, _ := uc.repo.Recovery(tor.Hash)
	cmd.Version = rec.Version
	cmd.Action = "apply_move"
	return cmd
}

func TestStorageMoveRenameAndPrefixConversion(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "rename", true: "conversion"}[legacy], func(t *testing.T) {
			uc, repo, done := setupTorrentUseCase(t)
			defer done()
			events := locationEvents(uc)
			data := bytes.Repeat([]byte("payload"), 400)
			tor, before := moveFixture(t, uc, legacy, data)
			cmd := planMoveFixture(t, uc, tor, before, events)
			if _, err := os.Stat(filepath.Join(uc.engine.DownloadDir(), "payload", "Moved")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("plan changed target", err)
			}
			if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
				t.Fatal(err)
			}
			waitLocationEvent(t, uc, tor.Hash, events, "location_completed")
			after, _ := repo.StorageMapping(tor.Hash)
			if after.Root != filepath.FromSlash("payload/Moved") || after.Prefix != "Original" || after.Revision != before.Revision+1 {
				t.Fatalf("mapping %+v", after)
			}
			actual, err := os.ReadFile(filepath.Join(uc.engine.DownloadDir(), after.Root, "video.bin"))
			if err != nil || !bytes.Equal(actual, data) {
				t.Fatal("bad moved payload", err)
			}
			if _, err := os.Stat(filepath.Join(uc.engine.DownloadDir(), before.Root)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("old tree survived successful cleanup", err)
			}
			current, _ := repo.GetTorrent(tor.Hash)
			if current.Control.DesiredRun != models.RunPaused {
				t.Fatal("move resumed torrent")
			}
			if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
				t.Fatal("apply replay failed", err)
			}
		})
	}
}

func TestStorageMoveRejectsChangedPlan(t *testing.T) {
	uc, repo, done := setupTorrentUseCase(t)
	defer done()
	events := locationEvents(uc)
	tor, m := moveFixture(t, uc, false, []byte("original"))
	cmd := planMoveFixture(t, uc, tor, m, events)
	path := filepath.Join(uc.engine.DownloadDir(), m.Root, "video.bin")
	if err := os.WriteFile(path, []byte("changed data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
		t.Fatal(err)
	}
	waitLocationEvent(t, uc, tor.Hash, events, "location_failed")
	rec, _ := repo.Recovery(tor.Hash)
	if rec.FailureCode != "candidate_changed" {
		t.Fatalf("wrong failure %+v", rec)
	}
	after, _ := repo.StorageMapping(tor.Hash)
	if after.Root != m.Root {
		t.Fatal("changed plan committed")
	}
	actual, _ := os.ReadFile(path)
	if string(actual) != "changed data" {
		t.Fatal("source overwritten")
	}
}

func TestStorageMoveDrainsTransfersAndCancelKeepsSource(t *testing.T) {
	uc, repo, done := setupTorrentUseCase(t)
	defer done()
	events := locationEvents(uc)
	tor, m := moveFixture(t, uc, true, []byte("preserve"))
	cmd := planMoveFixture(t, uc, tor, m, events)
	lease, err := uc.AcquireTransfer(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	defer lease()
	if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
		t.Fatal(err)
	}
	waitLocationEvent(t, uc, tor.Hash, events, "location_waiting_readers")
	if _, err := uc.AcquireTransfer(tor.Hash); !errors.Is(err, ErrStorageConflict) {
		t.Fatal("new transfer bypassed drain", err)
	}
	if err := uc.Pause(tor.Hash); err != nil {
		t.Fatal("pause blocked during drain", err)
	}
	cmd.Action = "cancel_storage"
	if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
		t.Fatal(err)
	}
	waitLocationEvent(t, uc, tor.Hash, events, "location_cancelled")
	after, _ := repo.StorageMapping(tor.Hash)
	if after.Root != m.Root || after.Revision != m.Revision {
		t.Fatal("cancel changed mapping")
	}
	data, err := os.ReadFile(filepath.Join(uc.engine.DownloadDir(), m.Root, "Original", "video.bin"))
	if err != nil || string(data) != "preserve" {
		t.Fatal("cancel lost source", err)
	}
}

func TestStorageCopyAllowsPauseAndUnrelatedAdd(t *testing.T) {
	uc, repo, done := setupTorrentUseCase(t)
	defer done()
	events := make(chan string, 128)
	copied := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	uc.recoveryStep = func(stage string) error {
		if stage == "location_file_copied" {
			close(copied)
			select {
			case <-release:
			case <-uc.engine.Context().Done():
				return context.Canceled
			}
		}
		select {
		case events <- stage:
		default:
		}
		return nil
	}
	tor, m := moveFixture(t, uc, true, bytes.Repeat([]byte("copy"), 1024))
	cmd := planMoveFixture(t, uc, tor, m, events)
	if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
		t.Fatal(err)
	}
	select {
	case <-copied:
	case <-time.After(10 * time.Second):
		t.Fatal("copy barrier not reached")
	}
	if err := uc.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	otherDone := make(chan error, 1)
	go func() {
		mi := migrationMetainfo(t, "Unrelated", []byte("other"), true, "v1")
		var data bytes.Buffer
		mi.Write(&data)
		_, err := uc.AddTorrentFile(&data)
		otherDone <- err
	}()
	select {
	case err := <-otherDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("copy held global admission lock")
	}
	cmd.Action = "cancel_storage"
	if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
		t.Fatal(err)
	}
	close(release)
	released = true
	waitLocationEvent(t, uc, tor.Hash, events, "location_cancelled")
	rec, _ := repo.Recovery(tor.Hash)
	j, err := decodeLocationJournal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(uc.engine.DownloadDir(), j.Staging, "video.bin")); err != nil {
		t.Fatal("cancel did not preserve staged copy", err)
	}
	snapshot, err := repo.ReadOwnership(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, claim := range snapshot.Claims {
		if claim.Path == j.Staging && claim.Kind == "retained" {
			found = true
		}
	}
	if !found {
		t.Fatal("staged copy lost ownership")
	}
}

func TestStorageMoveEXDEVAndPostRenameFlushRetry(t *testing.T) {
	for _, failure := range []string{"exdev", "flush"} {
		t.Run(failure, func(t *testing.T) {
			uc, repo, done := setupTorrentUseCase(t)
			defer done()
			events := locationEvents(uc)
			tor, m := moveFixture(t, uc, false, []byte("exact bytes"))
			uc.locationMove = func(root *os.Root, from, to string) error {
				if failure == "exdev" && from == m.Root {
					cause := syscall.EXDEV
					if runtime.GOOS == "windows" {
						cause = syscall.Errno(17) // ERROR_NOT_SAME_DEVICE
					}
					return &os.LinkError{Op: "rename", Old: from, New: to, Err: cause}
				}
				err := storage.MoveRootDirectoryNoReplace(root, from, to)
				if err == nil && failure == "flush" {
					return errors.Join(storage.ErrPublicationFlush, errors.New("injected flush failure"))
				}
				return err
			}
			cmd := planMoveFixture(t, uc, tor, m, events)
			if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
				t.Fatal(err)
			}
			if failure == "flush" {
				waitLocationEvent(t, uc, tor.Hash, events, "location_failed")
				rec, _ := repo.Recovery(tor.Hash)
				if rec.Stage != "publishing" || rec.FailureCode != "flush_failed" {
					t.Fatalf("lost publication intent %+v", rec)
				}
				cancel := cmd
				cancel.Action = "cancel_storage"
				if _, err := uc.StorageCommand(context.Background(), tor.Hash, cancel); !errors.Is(err, ErrStorageConflict) {
					t.Fatal("cancel accepted after publication", err)
				}
				cmd.Action = "retry_storage"
				if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
					t.Fatal(err)
				}
			}
			waitLocationEvent(t, uc, tor.Hash, events, "location_completed")
			after, _ := repo.StorageMapping(tor.Hash)
			data, err := os.ReadFile(filepath.Join(uc.engine.DownloadDir(), after.Root, "video.bin"))
			if err != nil || string(data) != "exact bytes" {
				t.Fatal("lost bytes", err)
			}
			if failure == "exdev" {
				rec, _ := repo.Recovery(tor.Hash)
				j, _ := decodeLocationJournal(rec)
				if j.Method != "copy" {
					t.Fatal("no copy fallback")
				}
			}
		})
	}
}

func TestStorageMoveSQLFailureAfterPublication(t *testing.T) {
	uc, repo, done := setupTorrentUseCase(t)
	defer done()
	events := locationEvents(uc)
	tor, m := moveFixture(t, uc, false, []byte("durable"))
	cmd := planMoveFixture(t, uc, tor, m, events)
	db, err := repository.NewSQLiteDB(filepath.Join(uc.engine.DownloadDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB().Exec(`CREATE TRIGGER fail_move_mapping BEFORE UPDATE OF root ON torrent_storage WHEN NEW.root='` + filepath.FromSlash("payload/Moved") + `' BEGIN SELECT RAISE(ABORT,'injected mapping commit');END`); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
		t.Fatal(err)
	}
	waitLocationEvent(t, uc, tor.Hash, events, "location_failed")
	unchanged, _ := repo.StorageMapping(tor.Hash)
	rec, _ := repo.Recovery(tor.Hash)
	if unchanged.Root != m.Root || unchanged.Revision != m.Revision || rec.Stage != "published" {
		t.Fatalf("partial SQL commit %+v %+v", unchanged, rec)
	}
	if _, err := db.DB().Exec(`DROP TRIGGER fail_move_mapping`); err != nil {
		t.Fatal(err)
	}
	cmd.Action = "retry_storage"
	if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
		t.Fatal(err)
	}
	waitLocationEvent(t, uc, tor.Hash, events, "location_completed")
	data, err := os.ReadFile(filepath.Join(uc.engine.DownloadDir(), filepath.FromSlash("payload/Moved/video.bin")))
	if err != nil || string(data) != "durable" {
		t.Fatal(err)
	}
}

func TestIsDirectoryNotEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "retained.bin"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.Remove(filepath.Base(dir)); err == nil || !isDirectoryNotEmpty(err) {
		t.Fatalf("native non-empty directory error not recognized: %v", err)
	}
	for _, err := range []error{nil, os.ErrPermission, os.ErrNotExist, errors.New("other failure"), &os.PathError{Op: "remove", Path: dir, Err: os.ErrPermission}} {
		if isDirectoryNotEmpty(err) {
			t.Fatalf("unrelated error accepted: %v", err)
		}
	}
}

func TestStorageMoveCleanupPreservesRecreatedSource(t *testing.T) {
	uc, repo, done := setupTorrentUseCase(t)
	defer done()
	tor, m := moveFixture(t, uc, true, []byte("old bytes"))
	events := make(chan string, 128)
	oldPath := filepath.Join(uc.engine.DownloadDir(), m.Root, "Original", "video.bin")
	uc.recoveryStep = func(stage string) error {
		if stage == "location_source_quarantined" {
			if err := os.WriteFile(oldPath, []byte("new external data"), 0600); err != nil {
				return err
			}
		}
		select {
		case events <- stage:
		default:
		}
		return nil
	}
	cmd := planMoveFixture(t, uc, tor, m, events)
	if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
		t.Fatal(err)
	}
	waitLocationEvent(t, uc, tor.Hash, events, "location_completed")
	data, err := os.ReadFile(oldPath)
	if err != nil || string(data) != "new external data" {
		t.Fatal("deleted replacement", err)
	}
	moved, err := os.ReadFile(filepath.Join(uc.engine.DownloadDir(), filepath.FromSlash("payload/Moved/video.bin")))
	if err != nil || string(moved) != "old bytes" {
		t.Fatal("lost moved bytes", err)
	}
	snapshot, err := repo.ReadOwnership(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, claim := range snapshot.Claims {
		if claim.Path == m.Root && claim.Kind == "retained" {
			found = true
		}
	}
	if !found {
		t.Fatal("replacement namespace became unowned")
	}
}
