package repository

import (
	"bufio"
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"torrent-stream-hub/internal/fastresume"
	"torrent-stream-hub/internal/models"
)

func resumeFixture(t testing.TB) (*SQLiteDB, *TorrentRepo, *StorageMapping, *fastresume.Checkpoint, ResumeToken) {
	t.Helper()
	db, err := NewSQLiteDB(filepath.Join(t.TempDir(), "resume.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r := NewTorrentRepo(db)
	if err := r.SaveTorrent(&models.Torrent{Hash: "canonical", Name: "test", State: models.StatePaused}); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureStorageMapping(&StorageMapping{Hash: "canonical", Version: 2, Root: filepath.FromSlash("payload/Test"), State: "ready"}); err != nil {
		t.Fatal(err)
	}
	m, err := r.StorageMapping("canonical")
	if err != nil {
		t.Fatal(err)
	}
	intent, err := r.GetIntent(m.Hash)
	if err != nil {
		t.Fatal(err)
	}
	c := &fastresume.Checkpoint{Schema: 1, Generation: 1, CreatedAtMS: 1000,
		Identity: fastresume.Identity{RecordID: intent.Version.RecordID, StorageID: m.StorageID, MappingRevision: m.Revision, LayoutVersion: m.Version, Root: m.Root,
			MetainfoDigest: strings.Repeat("a", 64), CatalogDigest: strings.Repeat("b", 64), VolumeID: "enrolled", PieceLength: 4, PieceCount: 3, ExtentLength: 12},
		Files: []fastresume.File{{Path: "file.bin", Length: 12, Evidence: fastresume.Evidence{Present: true, Identity: "file-id", Size: 12, ModifiedNS: 100}}}, Pieces: []byte{7}}
	token, err := r.CaptureResume(context.Background(), m, c.Identity.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	return db, r, m, c, token
}

func commitResumeFixture(t testing.TB, r *TorrentRepo, m *StorageMapping, c *fastresume.Checkpoint, token ResumeToken) ResumeToken {
	t.Helper()
	token, err := r.CommitResume(context.Background(), m, token, c)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestResumeReopenRevocationAndCAS(t *testing.T) {
	ctx := context.Background()
	db, r, m, c, token := resumeFixture(t)
	if s, err := r.LoadResume(ctx, m, token.RecordID); err != nil || !s.Invalidated || s.Checkpoint != nil {
		t.Fatal("new head is trusted", s, err)
	}
	token = commitResumeFixture(t, r, m, c, token)
	for range 2 {
		s, err := r.LoadResume(ctx, m, token.RecordID)
		if err != nil || s.Token != token || s.Invalidated || !bytes.Equal(s.Checkpoint.Pieces, []byte{7}) {
			t.Fatal("load consumes checkpoint", s, err)
		}
	}
	stale := token
	token, err := r.RevokeResume(ctx, m, token, []byte{2})
	if err != nil || token.Epoch != stale.Epoch+1 {
		t.Fatal(token, err)
	}
	c.Generation++
	if _, err := r.CommitResume(ctx, m, stale, c); !errors.Is(err, ErrStateConflict) {
		t.Fatal("stale seal cleared revocation", err)
	}
	if _, err := r.RevokeResume(ctx, m, stale, []byte{1}); !errors.Is(err, ErrStateConflict) {
		t.Fatal("stale revocation admitted", err)
	}
	again, err := r.RevokeResume(ctx, m, token, []byte{2})
	if err != nil || again != token {
		t.Fatal("repeated revoke amplified state", again, err)
	}
	var path string
	if err := db.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	r = NewTorrentRepo(reopened)
	s, err := r.LoadResume(ctx, m, token.RecordID)
	if err != nil || s.Token != token || !bytes.Equal(s.Revoked, []byte{2}) {
		t.Fatal("revocation lost after reopen", s, err)
	}
	filtered := fastresume.Filter(fastresume.Fast, s.Checkpoint, c.Identity, c.Files, s.Revoked, false)
	if !bytes.Equal(filtered.Bits, []byte{5}) {
		t.Fatal("revoked piece restored", filtered)
	}
	token = commitResumeFixture(t, r, m, c, token)
	s, err = r.LoadResume(ctx, m, token.RecordID)
	if err != nil || s.Checkpoint.Generation != 2 || !bytes.Equal(s.Revoked, []byte{0}) {
		t.Fatal("replacement not atomic", s, err)
	}
	_, err = r.InvalidateResume(ctx, m, token)
	if err != nil {
		t.Fatal(err)
	}
	s, err = r.LoadResume(ctx, m, token.RecordID)
	if err != nil || !s.Invalidated || s.Checkpoint != nil {
		t.Fatal("invalidation did not revoke all", s, err)
	}
}

func TestResumeUpgradeDoesNotAdoptHints(t *testing.T) {
	db, r, m, c, _ := resumeFixture(t)
	hash := []byte(strings.Repeat("x", 20))
	if _, err := db.DB().Exec(`INSERT INTO piece_completion(infohash,piece,complete) VALUES(?,0,1)`, hash); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the phase-42 boundary in this TEMP database, retaining all
	// application rows/hints and removing only phase-43 migration artifacts.
	for _, name := range []string{"resume_mapping_change", "resume_mapping_delete", "resume_record_delete", "resume_record_change", "resume_deletion_intent", "resume_journal_insert", "resume_journal_update", "resume_mapping_workflow"} {
		if _, err := db.DB().Exec(`DROP TRIGGER ` + name); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{`DROP TABLE resume_checkpoints`, `DROP TABLE resume_owners`, `DELETE FROM migrations WHERE name IN ('0020_resume_checkpoints.sql','0021_resume_mutation_fences.sql')`} {
		if _, err := db.DB().Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	var path string
	if err := db.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	r = NewTorrentRepo(upgraded)
	s, err := r.LoadResume(context.Background(), m, c.Identity.RecordID)
	if err != nil || s != nil {
		t.Fatal("upgrade promoted hints into checkpoint authority", s, err)
	}
	current, err := r.StorageMapping(m.Hash)
	if err != nil || current == nil || *current != *m {
		t.Fatal("upgrade changed mapping", current, err)
	}
	i, err := r.GetIntent(m.Hash)
	if err != nil || i.Version.RecordID != c.Identity.RecordID || i.Run != models.RunPaused {
		t.Fatal("upgrade changed record/intent", i, err)
	}
	var complete bool
	if err := upgraded.DB().QueryRow(`SELECT complete FROM piece_completion WHERE infohash=? AND piece=0`, hash).Scan(&complete); err != nil || !complete {
		t.Fatal("upgrade deleted hint", complete, err)
	}
}

func TestResumeFailureDoesNotAdmitWrite(t *testing.T) {
	db, r, m, c, token := resumeFixture(t)
	token = commitResumeFixture(t, r, m, c, token)
	path := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(path, []byte("old bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	// Fail AFTER the bitmap UPDATE, proving the rollback covers both rows.
	if _, err := db.DB().Exec(`CREATE TRIGGER fail_resume BEFORE UPDATE OF mutation_epoch ON resume_owners BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RevokeResume(context.Background(), m, token, []byte{1}); err == nil {
		_ = os.WriteFile(path, []byte("bad write"), 0600)
		t.Fatal("failed revocation admitted payload write")
	}
	s, err := r.LoadResume(context.Background(), m, token.RecordID)
	if err != nil || s.Token != token || !bytes.Equal(s.Revoked, []byte{0}) {
		t.Fatal("partial revocation committed", s, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "old bytes" {
		t.Fatal("payload mutated", err)
	}
	c.Generation++
	if _, err := r.CommitResume(context.Background(), m, token, c); err == nil {
		t.Fatal("failed seal committed")
	}
	s, err = r.LoadResume(context.Background(), m, token.RecordID)
	if err != nil || s.Checkpoint.Generation != 1 || s.Token != token {
		t.Fatal("old checkpoint damaged by failed seal", s, err)
	}
	if _, err := db.DB().Exec(`DROP TRIGGER fail_resume`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.RevokeResume(ctx, m, token, []byte{1}); err == nil {
		t.Fatal("canceled revoke succeeded")
	}
	if _, err := r.RevokeResume(context.Background(), m, token, []byte{1}); err != nil {
		t.Fatal("connection unusable after cancellation", err)
	}
}

func TestResumeIdentityFences(t *testing.T) {
	for _, mutation := range []string{"mapping", "alias", "deletion", "readd", "journal"} {
		t.Run(mutation, func(t *testing.T) {
			db, r, m, c, token := resumeFixture(t)
			token = commitResumeFixture(t, r, m, c, token)
			var err error
			switch mutation {
			case "mapping":
				_, err = db.DB().Exec(`UPDATE torrent_storage SET root=? WHERE hash=?`, filepath.FromSlash("payload/Other"), m.Hash)
			case "alias":
				if err := r.ClaimStorageAlias("alias", m.Hash); err != nil {
					t.Fatal(err)
				}
				if _, err := r.LoadResume(context.Background(), m, token.RecordID); err != nil {
					t.Fatal("consistent alias rejected", err)
				}
				_, err = db.DB().Exec(`UPDATE torrent_storage SET root=? WHERE hash='alias'`, filepath.FromSlash("payload/Other"))
			case "deletion":
				_, err = db.DB().Exec(`UPDATE torrents SET deletion_mode='payload' WHERE hash=?`, m.Hash)
			case "readd":
				_, err = db.DB().Exec(`DELETE FROM torrents WHERE hash=?`, m.Hash)
				if err == nil {
					err = r.SaveTorrent(&models.Torrent{Hash: m.Hash, Name: "replacement", State: models.StatePaused})
				}
			case "journal":
				_, err = db.DB().Exec(`INSERT INTO storage_recovery(hash,record_id,reason,stage) VALUES(?,?,'storage_move','copying')`, m.Hash, token.RecordID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.LoadResume(context.Background(), m, token.RecordID); !errors.Is(err, ErrStateConflict) {
				t.Fatal("stale load admitted", err)
			}
			c.Generation++
			if _, err := r.CommitResume(context.Background(), m, token, c); !errors.Is(err, ErrStateConflict) {
				t.Fatal("stale seal admitted", err)
			}
			if mutation == "readd" {
				current, _ := r.GetIntent(m.Hash)
				fresh, err := r.CaptureResume(context.Background(), m, current.Version.RecordID)
				if err != nil {
					t.Fatal(err)
				}
				if fresh.Epoch <= token.Epoch || fresh.RecordID == token.RecordID {
					t.Fatal("re-add inherited token", fresh)
				}
				s, err := r.LoadResume(context.Background(), m, fresh.RecordID)
				if err != nil || !s.Invalidated || s.Checkpoint != nil {
					t.Fatal("re-add inherited trust", s, err)
				}
			}
		})
	}
}

func TestResumeDeferredCommitFailure(t *testing.T) {
	db, r, m, c, token := resumeFixture(t)
	token = commitResumeFixture(t, r, m, c, token)
	for _, query := range []string{
		`CREATE TABLE resume_commit_parent(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE resume_commit_child(id INTEGER REFERENCES resume_commit_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
		`CREATE TRIGGER fail_resume_commit AFTER UPDATE OF mutation_epoch ON resume_owners BEGIN INSERT INTO resume_commit_child VALUES(1); END`,
	} {
		if _, err := db.DB().Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.RevokeResume(context.Background(), m, token, []byte{1}); err == nil {
		t.Fatal("expected deferred FK commit failure")
	}
	c.Generation++
	if _, err := r.CommitResume(context.Background(), m, token, c); err == nil {
		t.Fatal("expected seal commit failure")
	}
	s, err := r.LoadResume(context.Background(), m, token.RecordID)
	if err != nil || s.Token != token || s.Checkpoint.Generation != 1 || !bytes.Equal(s.Revoked, []byte{0}) {
		t.Fatal("failed commit was partially visible", s, err)
	}
	var children int
	if err := db.DB().QueryRow(`SELECT count(*) FROM resume_commit_child`).Scan(&children); err != nil || children != 0 {
		t.Fatal("transaction not rolled back", children, err)
	}
	if _, err := db.DB().Exec(`DROP TRIGGER fail_resume_commit`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RevokeResume(context.Background(), m, token, []byte{1}); err != nil {
		t.Fatal("connection retained failed transaction", err)
	}
}

func TestResumeCorruptionAndBounds(t *testing.T) {
	for _, column := range []string{"payload", "revoked", "integrity", "oversized"} {
		t.Run(column, func(t *testing.T) {
			db, r, m, c, token := resumeFixture(t)
			token = commitResumeFixture(t, r, m, c, token)
			query := `UPDATE resume_checkpoints SET ` + column + `=zeroblob(length(` + column + `))`
			if column == "revoked" {
				query = `UPDATE resume_checkpoints SET revoked=x'01'`
			}
			if column == "oversized" {
				db.DB().SetMaxOpenConns(1)
				if _, err := db.DB().Exec(`PRAGMA ignore_check_constraints=ON`); err != nil {
					t.Fatal(err)
				}
				query = `UPDATE resume_checkpoints SET payload=zeroblob(4194305)`
			}
			if _, err := db.DB().Exec(query); err != nil {
				t.Fatal(err)
			}
			if _, err := r.LoadResume(context.Background(), m, token.RecordID); err == nil {
				t.Fatal("corrupt checkpoint trusted")
			}
			if _, err := r.RevokeResume(context.Background(), m, token, []byte{1}); err == nil {
				t.Fatal("corrupt checkpoint granted write barrier")
			}
			if _, err := r.InvalidateResume(context.Background(), m, token); err != nil {
				t.Fatal("cannot revoke corrupt owner", err)
			}
		})
	}
}

func TestResumeConcurrentCAS(t *testing.T) {
	_, r, m, c, token := resumeFixture(t)
	token = commitResumeFixture(t, r, m, c, token)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, mask := range []byte{1, 2} {
		wg.Add(1)
		go func(mask byte) {
			defer wg.Done()
			<-start
			_, err := r.RevokeResume(context.Background(), m, token, []byte{mask})
			results <- err
		}(mask)
	}
	close(start)
	wg.Wait()
	close(results)
	passed, conflicts := 0, 0
	for err := range results {
		if err == nil {
			passed++
		} else if errors.Is(err, ErrStateConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if passed != 1 || conflicts != 1 {
		t.Fatal("CAS race", passed, conflicts)
	}
}

func TestResumeDurabilityEveryConnection(t *testing.T) {
	db, r, m, c, token := resumeFixture(t)
	// Multiple concurrently checked-out connections, then explicit replacement.
	var closeConns []func()
	for range 4 {
		conn, err := db.DB().Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		closeConns = append(closeConns, func() { conn.Close() })
		var level int
		if err := conn.QueryRowContext(context.Background(), `PRAGMA synchronous`).Scan(&level); err != nil || level != 2 {
			t.Fatal(level, err)
		}
	}
	for _, closeConn := range closeConns {
		closeConn()
	}
	db.DB().SetMaxOpenConns(1)
	conn, err := db.DB().Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `PRAGMA synchronous=OFF`); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if _, err := r.CommitResume(context.Background(), m, token, c); !errors.Is(err, ErrResumeDurability) {
		t.Fatal("unsafe pooled connection accepted", err)
	}
	conn, err = db.DB().Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	conn.Close()
	token = commitResumeFixture(t, r, m, c, token)
	if token.Generation != 1 {
		t.Fatal(token)
	}
	if _, err := db.DB().Exec(`UPDATE resume_owners SET mutation_epoch=?`, math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	s, err := readResumeHead(db.DB(), m.StorageID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.InvalidateResume(context.Background(), m, s.Token); !errors.Is(err, ErrStateConflict) {
		t.Fatal("epoch overflow", err)
	}
}

func TestResumeCrashRevocation(t *testing.T) {
	for _, stage := range []string{"before-revoke", "after-revoke", "after-write", "after-seal"} {
		t.Run(stage, func(t *testing.T) {
			db, r, m, c, token := resumeFixture(t)
			token = commitResumeFixture(t, r, m, c, token)
			var path string
			if err := db.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
				t.Fatal(err)
			}
			payloadPath := path + ".payload"
			if err := os.WriteFile(payloadPath, []byte("abcdefghijkl"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestResumeCrashHelper$")
			cmd.Env = append(os.Environ(), "TSH_RESUME_CRASH_DB="+path, "TSH_RESUME_CRASH_STAGE="+stage)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = os.Stderr
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
			line, err := bufio.NewReader(stdout).ReadString('\n')
			if err != nil || line != stage+"\n" {
				t.Fatalf("helper %q %v", line, err)
			}
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			waited = true
			reopened, err := NewSQLiteDB(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			s, err := NewTorrentRepo(reopened).LoadResume(context.Background(), m, token.RecordID)
			if err != nil {
				t.Fatal(err)
			}
			want := byte(0)
			if stage == "after-revoke" || stage == "after-write" {
				want = 1
			}
			if !bytes.Equal(s.Revoked, []byte{want}) {
				t.Fatal("revocation not durable after crash", s.Revoked)
			}
			generation := int64(1)
			if stage == "after-seal" {
				generation = 2
			}
			if s.Checkpoint.Generation != generation {
				t.Fatal("generation lost after crash", s.Checkpoint.Generation)
			}
			data, err := os.ReadFile(payloadPath)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "after-write" && string(data[:4]) != "WXYZ" || stage != "after-write" && string(data) != "abcdefghijkl" {
				t.Fatal("payload write ordering", string(data))
			}
		})
	}
}

func TestResumeCrashHelper(t *testing.T) {
	path := os.Getenv("TSH_RESUME_CRASH_DB")
	if path == "" {
		return
	}
	db, err := NewSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewTorrentRepo(db)
	m, err := r.StorageMapping("canonical")
	if err != nil {
		t.Fatal(err)
	}
	i, err := r.GetIntent(m.Hash)
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.LoadResume(context.Background(), m, i.Version.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	stage := os.Getenv("TSH_RESUME_CRASH_STAGE")
	if stage == "after-revoke" || stage == "after-write" {
		if _, err := r.RevokeResume(context.Background(), m, s.Token, []byte{1}); err != nil {
			t.Fatal(err)
		}
	}
	if stage == "after-seal" {
		s.Checkpoint.Generation++
		if _, err := r.CommitResume(context.Background(), m, s.Token, s.Checkpoint); err != nil {
			t.Fatal(err)
		}
	}
	if stage == "after-write" {
		f, err := os.OpenFile(path+".payload", os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteAt([]byte("WXYZ"), 0)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	os.Stdout.WriteString(stage + "\n")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestResumeWorkingSetCancellation(t *testing.T) {
	_, r, m, c, token := resumeFixture(t)
	for range cap(resumeWorkingSlots) {
		resumeWorkingSlots <- struct{}{}
	}
	defer func() {
		for range cap(resumeWorkingSlots) {
			<-resumeWorkingSlots
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.LoadResume(ctx, m, token.RecordID); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := r.CommitResume(ctx, m, token, c); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := r.RevokeResume(ctx, m, token, []byte{1}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// Small head operations need no serialized payload working set or writer
	// owned by a waiter; their SQL connection remains available.
	if _, err := r.CaptureResume(context.Background(), m, token.RecordID); err != nil {
		t.Fatal(err)
	}
}

func TestResumeSQLitePragmaOverride(t *testing.T) {
	db, err := NewSQLiteDB(filepath.Join(t.TempDir(), "pragma.db") + "?_pragma=synchronous(OFF)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for range 3 {
		conn, err := db.DB().Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var sync int
		if err := conn.QueryRowContext(context.Background(), `PRAGMA synchronous`).Scan(&sync); err != nil || sync != 2 {
			t.Fatal("DSN overrode FULL", sync, err)
		}
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		conn.Close()
	}
}

// Repository-only cost: two FULL/WAL commits per iteration, no payload flush,
// swarm or trusted runtime import. This is not an end-to-end resume benchmark.
func BenchmarkResumeDurableBarriers(b *testing.B) {
	b.StopTimer()
	_, r, m, c, token := resumeFixture(b)
	token = commitResumeFixture(b, r, m, c, token)
	b.ReportAllocs()
	b.ResetTimer()
	b.StartTimer()
	for range b.N {
		var err error
		token, err = r.RevokeResume(context.Background(), m, token, []byte{1})
		if err != nil {
			b.Fatal(err)
		}
		c.Generation = token.Generation + 1
		token, err = r.CommitResume(context.Background(), m, token, c)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(2, "commits/op")
}
