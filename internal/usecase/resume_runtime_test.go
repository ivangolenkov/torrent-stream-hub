package usecase

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/fastresume"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
)

func openResumeApplication(t *testing.T, dir string, mode fastresume.Mode, reads *atomic.Int64) (*TorrentUseCase, func()) {
	t.Helper()
	db, err := repository.NewSQLiteDB(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(&config.Config{DBPath: filepath.Join(dir, "hub.db"), DownloadDir: dir, TorrentPort: 0, MaxActiveDownloads: 1,
		BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true, BTDisableUTP: true, BTRetrackersMode: "off"},
		engine.WithStorageIOObserver(func(op, _ string, n int64) {
			if op == "read" && reads != nil {
				reads.Add(n)
			}
		}))
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	var uc *TorrentUseCase
	if mode == "" {
		uc = NewTorrentUseCase(e, repository.NewTorrentRepo(db))
	} else {
		uc, err = NewTorrentUseCaseWithResumeMode(e, repository.NewTorrentRepo(db), mode)
	}
	if err != nil {
		e.Close()
		_ = db.Close()
		t.Fatal(err)
	}
	close := func() { e.Close(); _ = db.Close() }
	t.Cleanup(close)
	return uc, close
}

func TestResumeRuntimeUpgradeHintsAndRecheckDiagnosis(t *testing.T) {
	dir := t.TempDir()
	warm, closeWarm := openResumeApplication(t, dir, fastresume.Fast, nil)
	hash, _, _ := warmResumeApplication(t, warm, "v1", bytes.Repeat([]byte("good"), 8192))
	closeWarm()
	db, err := repository.NewSQLiteDB(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"resume_checkpoints", "resume_owners"} {
		if _, err := db.DB().Exec("DELETE FROM " + table); err != nil {
			t.Fatal(err)
		}
	}
	rawHash, _ := hex.DecodeString(hash)
	for i := 0; i < 2; i++ {
		if _, err := db.DB().Exec(`INSERT OR REPLACE INTO piece_completion(infohash,piece,complete) VALUES(?,?,1)`, rawHash, i); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	var reads atomic.Int64
	cold, closeCold := openResumeApplication(t, dir, "", &reads) // public default, no opt-in
	if err := cold.RestoreTorrents(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cold.GetDownloadedFile(hash, 0); err != nil {
		t.Fatal(err)
	}
	view, err := cold.GetTorrent(hash)
	if err != nil {
		t.Fatal(err)
	}
	if reads.Load() < 32768 || view.Resume.Mode != "fast" || view.Resume.RestoredBytes != 0 || view.Resume.Reason != "no_checkpoint" {
		t.Fatalf("adopted disposable hints: %+v reads=%d", view.Resume, reads.Load())
	}
	closeCold()
	restored, closeRestored := openResumeApplication(t, dir, "", nil)
	defer closeRestored()
	if err := restored.RestoreTorrents(); err != nil {
		t.Fatal(err)
	}
	view, _ = restored.GetTorrent(hash)
	if view.Resume.RestoredBytes != 32768 || view.Resume.FreshlyVerifiedBytes != 0 {
		t.Fatal(view.Resume)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := restored.engine.GetRawTorrent(hash).VerifyDataContext(ctx); err != nil {
		t.Fatal(err)
	}
	view, _ = restored.GetTorrent(hash)
	if view.State != models.StatePaused || view.Resume.RestoredBytes != 0 || view.Resume.FreshlyVerifiedBytes != 32768 || view.Verification.CheckedBytes != 32768 {
		t.Fatalf("recheck double-counted restore or resumed: %+v", view)
	}
}

func TestResumeRuntimeStrictDetectsHiddenSameSizeEdit(t *testing.T) {
	dir := t.TempDir()
	warm, closeWarm := openResumeApplication(t, dir, fastresume.Fast, nil)
	hash, m, _ := warmResumeApplication(t, warm, "v1", bytes.Repeat([]byte("good"), 8192))
	closeWarm()
	path := filepath.Join(dir, m.Root, "video.bin")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteAt([]byte("evil"), 0)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int64
	strict, closeStrict := openResumeApplication(t, dir, fastresume.Verify, &reads)
	defer closeStrict()
	if err := strict.RestoreTorrents(); err != nil {
		t.Fatal(err)
	}
	raw := strict.engine.GetRawTorrent(hash)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		o := raw.FileObservations()[0]
		if o.CheckedBytes == 32768 && o.ActivePieces == 0 && o.QueuedPieces == 0 {
			if o.VerifiedBytes != 16384 || o.RestoredBytes != 0 || o.FreshlyVerifiedBytes != 16384 {
				t.Fatal(o)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if reads.Load() < 32768 {
		t.Fatal("strict skipped hashing", reads.Load())
	}
	closeStrict()
	var fastReads atomic.Int64
	reenabled, closeReenabled := openResumeApplication(t, dir, fastresume.Fast, &fastReads)
	defer closeReenabled()
	if err := reenabled.RestoreTorrents(); err != nil {
		t.Fatal(err)
	}
	raw = reenabled.engine.GetRawTorrent(hash)
	for {
		o := raw.FileObservations()[0]
		if o.CheckedBytes == 16384 && o.ActivePieces == 0 && o.QueuedPieces == 0 {
			if o.RestoredBytes != 16384 || o.FreshlyVerifiedBytes != 0 || o.VerifiedBytes != 16384 {
				t.Fatal("re-enable revived rejected checkpoint", o)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if fastReads.Load() != 16384 {
		t.Fatal("re-enable hashed trusted subset", fastReads.Load())
	}
	if _, err := raw.Piece(0).Storage().ReadAt(make([]byte, 4), 0); !errors.Is(err, storage.ErrUnverifiedPayload) {
		t.Fatal("strictly rejected data readable after fast re-enable", err)
	}
	view, err := reenabled.GetTorrent(hash)
	if err != nil || view.State != models.StatePaused {
		t.Fatal("mode switch resumed paused intent", view, err)
	}
}

func warmResumeApplication(t *testing.T, uc *TorrentUseCase, version string, data []byte) (string, *repository.StorageMapping, string) {
	t.Helper()
	mi := migrationMetainfo(t, "Resume", data, true, version)
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
	m, _ := uc.repo.StorageMapping(tor.Hash)
	info, _ := mi.UnmarshalInfo()
	for _, file := range info.UpvertedFiles() {
		path, err := m.Layout().File(&info, &file)
		if err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(uc.engine.DownloadDir(), path)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if ready, err := uc.inspectInventory(context.Background(), tor.Hash, false); err != nil || !ready {
		t.Fatal(ready, err)
	}
	saved, _ := uc.repo.GetTorrent(tor.Hash)
	if err := uc.restoreInactiveLocked(saved); err != nil {
		t.Fatal(err)
	}
	intent, _ := uc.repo.GetIntent(tor.Hash)
	if err := bindVerificationRuntime(uc.engine, uc.repo, intent.Version); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := uc.engine.GetRawTorrent(tor.Hash).VerifyDataContext(ctx); err != nil {
		t.Fatal(err)
	}
	return tor.Hash, m, intent.Version.RecordID
}

func TestResumeRuntimeShutdownAndRepeatedCleanRestore(t *testing.T) {
	for _, version := range []string{"v1", "v2", "hybrid"} {
		t.Run(version, func(t *testing.T) {
			dir := t.TempDir()
			data := bytes.Repeat([]byte("good"), 8192)
			warm, closeWarm := openResumeApplication(t, dir, fastresume.Fast, nil)
			hash, m, record := warmResumeApplication(t, warm, version, data)
			if _, dirty := warm.engine.GetRawTorrent(hash).FileCheckpointState(); !dirty {
				t.Fatal("warm fixture not dirty")
			}
			// No manual queueing or SealResume: the installed final scan must
			// catch this owner even before its first scanner tick.
			closeWarm()
			for i := 0; i < 2; i++ {
				var reads atomic.Int64
				cold, closeCold := openResumeApplication(t, dir, fastresume.Fast, &reads)
				saved, err := cold.repo.LoadResume(context.Background(), m, record)
				if err != nil || saved == nil || saved.Invalidated || saved.Token.Generation < 1 {
					t.Fatal("automatic shutdown seal missing", saved, err)
				}
				if err := cold.RestoreTorrents(); err != nil {
					t.Fatal(err)
				}
				if reads.Load() != 0 {
					t.Fatal("trusted startup hashed payload", reads.Load())
				}
				for poll := 0; poll < 3; poll++ {
					view, err := cold.GetTorrent(hash)
					if err != nil || view == nil || view.Resume == nil {
						t.Fatal("missing resume diagnosis", view, err)
					}
					d := view.Resume
					if d.Mode != "fast" || d.Status != "restored" || d.RestoredBytes != int64(len(data)) || d.FreshlyVerifiedBytes != 0 || d.CheckpointGeneration == "" || d.CheckpointAtMS == 0 {
						t.Fatalf("wrong restore diagnosis: %+v", d)
					}
					if view.Verification.CheckedBytes != 0 {
						t.Fatal("restore impersonates hashing", view.Verification)
					}
					compact := CompactTorrents([]*models.Torrent{view})[0]
					if compact.Files != nil || compact.Resume != d {
						t.Fatal("compact lost resume or duplicated files")
					}
				}
				if reads.Load() != 0 {
					t.Fatal("polling read payload", reads.Load())
				}
				raw := cold.engine.GetRawTorrent(hash)
				if raw == nil {
					t.Fatal("runtime not restored")
				}
				if _, dirty := raw.FileCheckpointState(); dirty {
					t.Fatal("restore scheduled a redundant seal")
				}
				tor, file, err := cold.GetDownloadedFile(hash, 0)
				if err != nil || tor.State != models.StatePaused {
					t.Fatal("restored data not readable while paused", err)
				}
				if reads.Load() != 0 {
					t.Fatal("readiness hashed trusted payload", reads.Load())
				}
				f, err := file.Open()
				if err != nil {
					t.Fatal(err)
				}
				actual, err := io.ReadAll(f)
				_ = f.Close()
				if err != nil || !bytes.Equal(actual, data) {
					t.Fatal("restored payload changed", err)
				}
				closeCold()
			}
		})
	}
}

func checkRuntimeResumeSubset(t *testing.T, uc *TorrentUseCase, hash string, partial bool, reads *atomic.Int64) {
	t.Helper()
	if err := uc.RestoreTorrents(); err != nil {
		t.Fatal(err)
	}
	raw := uc.engine.GetRawTorrent(hash)
	if raw == nil {
		t.Fatal("runtime missing")
	}
	var expectedHashBytes int64
	if partial {
		expectedHashBytes = 16384 // only the untrusted, corrupt second piece
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for {
			state := raw.PieceState(1)
			if !state.Checking && !state.Marking && reads.Load() >= expectedHashBytes {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("untrusted remainder was not checked")
			case <-time.After(time.Millisecond):
			}
		}
	}
	if reads.Load() != expectedHashBytes {
		t.Fatal("startup hashed outside untrusted subset", reads.Load(), expectedHashBytes)
	}
	if partial {
		if _, err := raw.Piece(1).Storage().ReadAt(make([]byte, 4), 0); !errors.Is(err, storage.ErrUnverifiedPayload) {
			t.Fatal("unsealed piece became readable", err)
		}
	}
	r := raw.NewReader()
	defer r.Close()
	r.SetReadahead(4)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var prefix [4]byte
	if n, err := r.ReadContext(ctx, prefix[:]); err != nil || n != 4 || string(prefix[:]) != "good" {
		t.Fatal("trusted prefix unavailable", n, err)
	}
	if reads.Load() != expectedHashBytes+4 {
		t.Fatal("reader performed extra startup hashing", reads.Load())
	}
}

func TestResumeRuntimePartialShutdown(t *testing.T) {
	for _, version := range []string{"v1", "v2", "hybrid"} {
		t.Run(version, func(t *testing.T) {
			dir := t.TempDir()
			warm, closeWarm := openResumeApplication(t, dir, fastresume.Fast, nil)
			hash, m, record := warmResumeApplication(t, warm, version, bytes.Repeat([]byte("good"), 8192))
			if _, err := warm.engine.GetRawTorrent(hash).Piece(1).Storage().WriteAt([]byte("evil"), 0); err != nil {
				t.Fatal(err)
			}
			closeWarm()
			var reads atomic.Int64
			cold, closeCold := openResumeApplication(t, dir, fastresume.Fast, &reads)
			defer closeCold()
			saved, err := cold.repo.LoadResume(context.Background(), m, record)
			if err != nil || saved == nil || saved.Invalidated || !bytes.Equal(saved.Checkpoint.Pieces, []byte{1}) {
				t.Fatal("partial checkpoint lost its subset", saved, err)
			}
			checkRuntimeResumeSubset(t, cold, hash, true, &reads)
		})
	}
}

func TestResumeRuntimeCrashHelper(t *testing.T) {
	if os.Getenv("TSH_RESUME_RUNTIME_CHILD") != "1" {
		return
	}
	uc, closeWarm := openResumeApplication(t, os.Getenv("TSH_RESUME_RUNTIME_DIR"), fastresume.Fast, nil)
	hash, _, _ := warmResumeApplication(t, uc, os.Getenv("TSH_RESUME_RUNTIME_VERSION"), bytes.Repeat([]byte("good"), 8192))
	if os.Getenv("TSH_RESUME_RUNTIME_PARTIAL") == "1" {
		if _, err := uc.engine.GetRawTorrent(hash).Piece(1).Storage().WriteAt([]byte("evil"), 0); err != nil {
			t.Fatal(err)
		}
	}
	release, err := uc.lockRecord(hash, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m, _ := uc.repo.StorageMapping(hash)
	_, mapping, source, err := uc.productionResumeSource(context.Background(), m.StorageID)
	if err == nil {
		err = uc.repo.SealResume(context.Background(), mapping, source)
	}
	release()
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TSH_RESUME_RUNTIME_PREATTACH") == "1" {
		closeWarm()
		cold, _ := openResumeApplication(t, os.Getenv("TSH_RESUME_RUNTIME_DIR"), fastresume.Fast, nil)
		cold.engine.SetResumeMutationFactory(func(ctx context.Context, info *metainfo.Info, hash metainfo.Hash) (*storage.FileResumeHooks, error) {
			hooks, err := cold.resumeMutationHooks(ctx, info, hash)
			if err != nil || hooks == nil || hooks.Restore == nil {
				t.Fatal("no restore candidate before attach", err)
			}
			restore := hooks.Restore
			hooks.Restore = func(ctx context.Context, backend storage.TorrentImpl) error {
				if err := restore(ctx, backend); err != nil {
					return err
				}
				if backend.CachedRestored == nil || !backend.CachedRestored(0) {
					t.Fatal("pre-attach import absent")
				}
				fmt.Println("RESUME_RUNTIME_SEALED")
				select {} // Imported, but the library has not published this backend.
			}
			return hooks, nil
		})
		if err := cold.RestoreTorrents(); err != nil {
			t.Fatal(err)
		}
		t.Fatal("pre-attach barrier did not stop publication")
	}
	fmt.Println("RESUME_RUNTIME_SEALED")
	select {} // Parent kills the process; no usecase/engine/SQLite cleanup runs.
}

func TestResumeRuntimeCrashReopen(t *testing.T) {
	testResumeRuntimeCrashReopen(t, false)
}

func TestResumeRuntimeCrashBeforeAttach(t *testing.T) {
	testResumeRuntimeCrashReopen(t, true)
}

func testResumeRuntimeCrashReopen(t *testing.T, beforeAttach bool) {
	for _, version := range []string{"v1", "v2", "hybrid"} {
		for _, partial := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/partial=%t", version, partial), func(t *testing.T) {
				dir := t.TempDir()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestResumeRuntimeCrashHelper$")
				cmd.Env = append(os.Environ(), "TSH_RESUME_RUNTIME_CHILD=1", "TSH_RESUME_RUNTIME_DIR="+dir, "TSH_RESUME_RUNTIME_VERSION="+version)
				if beforeAttach {
					cmd.Env = append(cmd.Env, "TSH_RESUME_RUNTIME_PREATTACH=1")
				}
				if partial {
					cmd.Env = append(cmd.Env, "TSH_RESUME_RUNTIME_PARTIAL=1")
				}
				out, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				defer cmd.Process.Kill()
				scanner := bufio.NewScanner(out)
				ready := false
				for scanner.Scan() {
					if scanner.Text() == "RESUME_RUNTIME_SEALED" {
						ready = true
						break
					}
				}
				if !ready {
					_ = cmd.Wait()
					t.Fatal("child did not seal", scanner.Err(), stderr.String())
				}
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				_ = cmd.Wait()
				var reads atomic.Int64
				cold, closeCold := openResumeApplication(t, dir, fastresume.Fast, &reads)
				defer closeCold()
				torrents, err := cold.repo.GetAllTorrents()
				if err != nil || len(torrents) != 1 {
					t.Fatal(len(torrents), err)
				}
				checkRuntimeResumeSubset(t, cold, torrents[0].Hash, partial, &reads)
			})
		}
	}
}

func TestResumeRuntimePeriodicSealWithReaderAndWrites(t *testing.T) {
	uc, closeUC := openResumeApplication(t, t.TempDir(), fastresume.Fast, nil)
	defer closeUC()
	hash, m, record := warmResumeApplication(t, uc, "v1", bytes.Repeat([]byte("good"), 8192))
	raw := uc.engine.GetRawTorrent(hash)
	piece := raw.Piece(1).Storage()
	if _, err := piece.WriteAt([]byte("evil"), 0); err != nil {
		t.Fatal(err)
	}
	r := raw.NewReader()
	defer r.Close()
	r.SetReadahead(4)
	ctx, cancel := context.WithTimeout(context.Background(), fastresume.CheckpointInterval+15*time.Second)
	defer cancel()
	var prefix [4]byte
	if n, err := r.ReadContext(ctx, prefix[:]); err != nil || n != 4 || string(prefix[:]) != "good" {
		t.Fatal(n, err)
	}
	writeCtx, stopWrites := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var writes atomic.Int64
	go func() {
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-writeCtx.Done():
				done <- nil
				return
			case <-tick.C:
				if _, err := piece.WriteAt([]byte("evil"), 0); err != nil {
					done <- err
					return
				}
				writes.Add(1)
			}
		}
	}()
	defer func() {
		stopWrites()
		if err := <-done; err != nil {
			t.Error("writer failed during online seal", err)
		}
	}()
	// Use the actual installed scanner and 60-second ticker, not a manual seal
	// or shutdown. The reader remains open and the writer keeps submitting I/O.
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		saved, err := uc.repo.LoadResume(ctx, m, record)
		if err != nil {
			t.Fatal(err)
		}
		if saved != nil && !saved.Invalidated && saved.Token.Generation > 0 {
			if !bytes.Equal(saved.Checkpoint.Pieces, []byte{1}) {
				t.Fatal("periodic seal included unverified writes", saved.Checkpoint.Pieces)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("installed periodic producer did not seal", ctx.Err())
		case <-tick.C:
		}
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if n, err := r.ReadContext(ctx, prefix[:]); err != nil || n != 4 || string(prefix[:]) != "good" {
		t.Fatal("online seal disconnected existing reader", n, err)
	}
	before := writes.Load()
	for writes.Load() == before {
		select {
		case <-ctx.Done():
			t.Fatal("online seal starved writer")
		case <-tick.C:
		}
	}
}

func TestResumeRuntimeVerifyAndEvidenceFallback(t *testing.T) {
	for _, mode := range []fastresume.Mode{fastresume.Fast, fastresume.Verify} {
		t.Run(string(mode), func(t *testing.T) {
			dir := t.TempDir()
			warm, closeWarm := openResumeApplication(t, dir, fastresume.Fast, nil)
			hash, m, _ := warmResumeApplication(t, warm, "v1", bytes.Repeat([]byte("good"), 8192))
			closeWarm()
			if mode == fastresume.Fast {
				// A changed timestamp is evidence, not a content hash. Fast must
				// withdraw this file and fall back even though bytes are unchanged.
				path := filepath.Join(dir, m.Root, "video.bin")
				when := time.Now().Add(2 * time.Hour)
				if err := os.Chtimes(path, when, when); err != nil {
					t.Fatal(err)
				}
			}
			var reads atomic.Int64
			cold, closeCold := openResumeApplication(t, dir, mode, &reads)
			defer closeCold()
			if err := cold.RestoreTorrents(); err != nil {
				t.Fatal(err)
			}
			_, _, err := cold.GetDownloadedFile(hash, 0)
			if err != nil {
				t.Fatal("fallback prevented ordinary verification", err)
			}
			if reads.Load() < 32768 {
				t.Fatal("strict/evidence fallback trusted old data", reads.Load())
			}
			view, err := cold.GetTorrent(hash)
			if err != nil || view.Resume == nil {
				t.Fatal(view, err)
			}
			if view.Resume.RestoredBytes != 0 || view.Resume.FreshlyVerifiedBytes != 32768 {
				t.Fatalf("fallback counters: %+v", view.Resume)
			}
			if mode == fastresume.Verify && (view.Resume.Status != "disabled" || view.Resume.Reason != "strict_mode") {
				t.Fatalf("strict diagnosis: %+v", view.Resume)
			}
			if mode == fastresume.Fast && (view.Resume.Status != "fallback" || view.Resume.Reason != "files_changed") {
				t.Fatalf("evidence diagnosis: %+v", view.Resume)
			}
		})
	}
}

func TestResumeRuntimeRejectedSecondInstallerPreservesOwnership(t *testing.T) {
	uc, _, done := setupTorrentUseCase(t)
	defer done()
	producer := uc.resumeProducer
	if producer == nil || uc.resumeMode != fastresume.Fast {
		t.Fatal("default constructor did not install fast producer")
	}
	if err := uc.installResumeProducer(); err == nil || uc.resumeProducer != producer {
		t.Fatal("second installer stole the finalizer")
	}
	release, err := uc.lockRecord("still-open", context.Background())
	if err != nil {
		t.Fatal("failed installer closed the existing coordinator", err)
	}
	release()
}

func TestResumeRuntimeFailedFallbackBarrierRejectsOpen(t *testing.T) {
	dir := t.TempDir()
	warm, closeWarm := openResumeApplication(t, dir, fastresume.Fast, nil)
	hash, m, _ := warmResumeApplication(t, warm, "v1", bytes.Repeat([]byte("good"), 8192))
	closeWarm()
	path := filepath.Join(dir, m.Root, "video.bin")
	when := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int64
	cold, closeCold := openResumeApplication(t, dir, fastresume.Fast, &reads)
	defer closeCold()
	unblock := failResumeMutationSQL(t, cold)
	defer unblock()
	if err := cold.RestoreTorrents(); err == nil {
		t.Fatal("failed revoke/invalidation admitted ordinary verification")
	}
	if cold.engine.GetRawTorrent(hash) != nil || reads.Load() != 0 {
		t.Fatal("failed open published a runtime or read payload", reads.Load())
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, bytes.Repeat([]byte("good"), 8192)) {
		t.Fatal("failed fallback mutated payload", err)
	}
}
