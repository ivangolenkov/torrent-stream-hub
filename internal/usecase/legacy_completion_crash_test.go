package usecase

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"torrent-stream-hub/internal/fastresume"
	"torrent-stream-hub/internal/repository"
)

func TestResumeRuntimeLegacyHintsCrashHelper(t *testing.T) {
	if os.Getenv("TSH_LEGACY_HINT_CHILD") != "1" {
		return
	}
	dir := os.Getenv("TSH_LEGACY_HINT_DIR")
	uc, _ := openResumeApplication(t, dir, fastresume.Fast, nil, true)
	warmResumeApplication(t, uc, os.Getenv("TSH_LEGACY_HINT_VERSION"), bytes.Repeat([]byte("good"), 8192))
	db, err := repository.NewSQLiteDB(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Observe an actual committed SQLite hint batch, not merely pending=0
	// (flush removes pending entries before its transaction commits).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var count int
		if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM piece_completion WHERE complete=1`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count >= 2 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("hints did not persist", ctx.Err())
		case <-ticker.C:
		}
	}
	// Test-only setup of the no-checkpoint crash boundary. The production
	// periodic producer's 60-second tick cannot occur within the parent budget.
	for _, table := range []string{"resume_checkpoints", "resume_owners"} {
		if _, err := db.DB().Exec("DELETE FROM " + table); err != nil {
			t.Fatal(err)
		}
	}
	fmt.Println("LEGACY_HINTS_WITHOUT_CHECKPOINT")
	select {} // Parent kills the process: no engine shutdown/final seal/Close.
}

func TestResumeRuntimeLegacyHintsCrashReopen(t *testing.T) {
	for _, version := range []string{"v1", "v2", "hybrid"} {
		for _, corrupt := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/corrupt=%t", version, corrupt), func(t *testing.T) {
				dir := t.TempDir()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestResumeRuntimeLegacyHintsCrashHelper$")
				cmd.Env = append(os.Environ(), "TSH_LEGACY_HINT_CHILD=1", "TSH_LEGACY_HINT_DIR="+dir, "TSH_LEGACY_HINT_VERSION="+version)
				out, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				waited := false
				defer func() {
					if !waited {
						_ = cmd.Process.Kill()
						_ = cmd.Wait()
					}
				}()
				scanner := bufio.NewScanner(out)
				var stdout bytes.Buffer
				ready := false
				for scanner.Scan() {
					stdout.WriteString(scanner.Text() + "\n")
					if scanner.Text() == "LEGACY_HINTS_WITHOUT_CHECKPOINT" {
						ready = true
						break
					}
				}
				if !ready {
					_ = cmd.Wait()
					waited = true
					t.Fatal("crash boundary not reached", scanner.Err(), stdout.String(), stderr.String())
				}
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				_ = cmd.Wait()
				waited = true
				db, err := repository.NewSQLiteDB(filepath.Join(dir, "hub.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				var hints, checkpoints int
				if err := db.DB().QueryRow(`SELECT COUNT(*) FROM piece_completion WHERE complete=1`).Scan(&hints); err != nil {
					t.Fatal(err)
				}
				if err := db.DB().QueryRow(`SELECT COUNT(*) FROM resume_checkpoints`).Scan(&checkpoints); err != nil {
					t.Fatal(err)
				}
				if hints < 2 || checkpoints != 0 {
					t.Fatal("fixture did not persist untrusted hints alone", hints, checkpoints)
				}
				data := bytes.Repeat([]byte("good"), 8192)
				mi := migrationMetainfo(t, "Resume", data, true, version)
				// Pure v2 uses the application's canonical SHA-256 identity, not
				// MetaInfo.HashInfoBytes' SHA-1. Read the actual persisted record.
				repo := repository.NewTorrentRepo(db)
				records, err := repo.GetAllTorrents()
				if err != nil || len(records) != 1 {
					t.Fatal(len(records), err)
				}
				hash := records[0].Hash
				if corrupt {
					mapping, err := repo.StorageMapping(hash)
					if err != nil || mapping == nil {
						t.Fatal(mapping, err)
					}
					info, err := mi.UnmarshalInfo()
					if err != nil {
						t.Fatal(err)
					}
					fi := info.UpvertedFiles()[0]
					rel, err := mapping.Layout().File(&info, &fi)
					if err != nil {
						t.Fatal(err)
					}
					// Model lost/changed payload bytes with the SAME logical size.
					// A process kill alone cannot simulate power-loss/page-cache loss.
					if err := os.WriteFile(filepath.Join(dir, rel), bytes.Repeat([]byte("X"), len(data)), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				var reads atomic.Int64
				cold, closeCold := openResumeApplication(t, dir, fastresume.Fast, &reads, true)
				defer closeCold()
				if err := cold.RestoreTorrents(); err != nil {
					t.Fatal(err)
				}
				verify, stop := context.WithTimeout(context.Background(), 10*time.Second)
				defer stop()
				raw := cold.engine.GetRawTorrent(hash)
				if raw == nil {
					t.Fatal("record did not attach", hash)
				}
				if err := raw.VerifyDataContext(verify); err != nil {
					t.Fatal(err)
				}
				view, err := cold.GetTorrent(hash)
				if err != nil {
					t.Fatal(err)
				}
				if reads.Load() < int64(len(data)) || view.Resume.RestoredBytes != 0 || view.Resume.Reason != "no_checkpoint" {
					t.Fatal("durable hints bypassed hashing", reads.Load(), view.Resume)
				}
				want := int64(len(data))
				if corrupt {
					want = 0
				}
				if view.Downloaded != want || view.Resume.FreshlyVerifiedBytes != want {
					t.Fatal("hint overrode real hash result", view.Downloaded, view.Resume)
				}
			})
		}
	}
}
