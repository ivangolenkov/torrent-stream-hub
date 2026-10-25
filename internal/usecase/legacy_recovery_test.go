package usecase

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"os"
	"path/filepath"
	"testing"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"
)

func customLegacyConflict(t *testing.T, uc *TorrentUseCase, info metainfo.Info, contents map[string][]byte) string {
	t.Helper()
	encoded, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	var input bytes.Buffer
	if err := (&metainfo.MetaInfo{InfoBytes: encoded}).Write(&input); err != nil {
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
	for path, data := range contents {
		physical, err := m.Layout().Path(path)
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(uc.engine.DownloadDir(), physical)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := uc.restorePersistedTorrent(context.Background(), tor.Hash); err == nil {
		t.Fatal("conflict not detected")
	}
	return tor.Hash
}

func TestLegacyRecoveryAmbiguousBoundaryAndZeroLength(t *testing.T) {
	for _, missingNeighbor := range []bool{false, true} {
		t.Run(fmt.Sprintf("boundary/missing=%t", missingNeighbor), func(t *testing.T) {
			uc, repo, cleanup := setupTorrentUseCase(t)
			defer cleanup()
			first, last := sha1.Sum([]byte("abcd")), sha1.Sum([]byte("ef"))
			info := metainfo.Info{Name: "boundary", PieceLength: 4, Files: []metainfo.FileInfo{{Length: 3, Path: []string{"a"}}, {Length: 3, Path: []string{"b"}}}, Pieces: append(first[:], last[:]...)}
			contents := map[string][]byte{"boundary/a": []byte("xxx"), "boundary/a.part": []byte("abc")}
			if !missingNeighbor {
				contents["boundary/b"] = []byte("def")
			}
			hash := customLegacyConflict(t, uc, info, contents)
			rec, _ := repo.Recovery(hash)
			if err := uc.analyzeLegacy(context.Background(), hash, rec.RecordID, rec.Version); err != nil {
				t.Fatal(err)
			}
			rec, _ = repo.Recovery(hash)
			j, _ := decodeLegacyJournal(rec)
			want := "part"
			if missingNeighbor {
				want = ""
			}
			if j.Analysis.Recommended != want {
				t.Fatalf("shared boundary evidence ignored: %+v", j.Analysis)
			}
			if missingNeighbor {
				if _, err := os.Stat(filepath.Join(uc.engine.DownloadDir(), "payload", "boundary", "b")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("analysis created neighbor", err)
				}
			}
		})
	}
	t.Run("different-incomplete", func(t *testing.T) {
		uc, repo, cleanup := setupTorrentUseCase(t)
		defer cleanup()
		first, last := sha1.Sum([]byte("abcd")), sha1.Sum([]byte("efgh"))
		hash := customLegacyConflict(t, uc, metainfo.Info{Name: "partial", Length: 8, PieceLength: 4, Pieces: append(first[:], last[:]...)}, map[string][]byte{"partial": []byte("abcdBAD!"), "partial.part": []byte("BAD!efgh")})
		rec, _ := repo.Recovery(hash)
		if err := uc.analyzeLegacy(context.Background(), hash, rec.RecordID, rec.Version); err != nil {
			t.Fatal(err)
		}
		rec, _ = repo.Recovery(hash)
		j, _ := decodeLegacyJournal(rec)
		if j.Analysis.Recommended != "" || j.Analysis.Final.ValidPieces != 1 || j.Analysis.Part.ValidPieces != 1 {
			t.Fatalf("partial pieces falsely selected or merged: %+v", j.Analysis)
		}
	})
	t.Run("zero-length", func(t *testing.T) {
		uc, repo, cleanup := setupTorrentUseCase(t)
		defer cleanup()
		hash := customLegacyConflict(t, uc, metainfo.Info{Name: "empty", PieceLength: 4}, map[string][]byte{"empty": {}, "empty.part": {}})
		rec, _ := repo.Recovery(hash)
		if err := uc.analyzeLegacy(context.Background(), hash, rec.RecordID, rec.Version); err != nil {
			t.Fatal(err)
		}
		rec, _ = repo.Recovery(hash)
		j, _ := decodeLegacyJournal(rec)
		if !j.Analysis.Equal || j.Analysis.Final.TotalPieces != 0 {
			t.Fatalf("bad empty analysis %+v", j.Analysis)
		}
		if err := uc.applyLegacy(context.Background(), hash, rec.RecordID, rec.Version, "final"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestLegacyRecoveryFlushDiskAndTargetFailures(t *testing.T) {
	for _, failure := range []string{"flush_after_move", "disk_full", "target_exists"} {
		t.Run(failure, func(t *testing.T) {
			uc, repo, cleanup := setupTorrentUseCase(t)
			defer cleanup()
			hash := legacyConflictFixture(t, uc, "v1", []byte("damaged!"), []byte("original"))
			rec, _ := repo.Recovery(hash)
			if err := uc.analyzeLegacy(context.Background(), hash, rec.RecordID, rec.Version); err != nil {
				t.Fatal(err)
			}
			rec, _ = repo.Recovery(hash)
			uc.recoveryMove = func(root *os.Root, from, to string) error {
				switch failure {
				case "flush_after_move":
					if err := storage.MoveRootNoReplace(root, from, to); err != nil {
						return err
					}
					return storage.ErrPublicationFlush
				case "disk_full":
					return engine.ErrMigrationDiskFull
				default:
					if err := os.WriteFile(filepath.Join(root.Name(), to), []byte("foreign"), 0600); err != nil {
						return err
					}
					return os.ErrExist
				}
			}
			err := uc.applyLegacy(context.Background(), hash, rec.RecordID, rec.Version, "part")
			if err == nil {
				t.Fatal("injected failure ignored")
			}
			uc.saveRecoveryFailure(hash, rec.RecordID, rec.Version, err)
			saved, _ := repo.GetTorrent(hash)
			want := map[string]string{"flush_after_move": "flush_failed", "disk_full": "disk_full", "target_exists": "target_exists"}[failure]
			if saved.StorageRecovery.FailureCode != want {
				t.Fatalf("missing specific failure code: %+v", saved.StorageRecovery)
			}
			if failure == "flush_after_move" {
				uc.recoveryMove = nil
				if err := uc.applyLegacy(context.Background(), hash, rec.RecordID, rec.Version, "part"); err != nil {
					t.Fatal("flush retry lost moved copy", err)
				}
			}
		})
	}
}

func legacyConflictFixture(t *testing.T, uc *TorrentUseCase, format string, final, part []byte) string {
	t.Helper()
	tor, source := legacyMigrationFixture(t, uc, migrationMetainfo(t, "movie", []byte("original"), format == "hybrid", format), final, false)
	rel, err := filepath.Rel(uc.engine.DownloadDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(uc.engine.DownloadDir(), payload.Root(tor.Hash), rel)
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(source, target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target+".part", part, 0600); err != nil {
		t.Fatal(err)
	}
	if err := uc.repo.SaveStorageMapping(&repository.StorageMapping{Hash: tor.Hash, Root: payload.Root(tor.Hash), Version: payload.LayoutVersion, State: "ready"}); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.restorePersistedTorrent(context.Background(), tor.Hash); err == nil {
		t.Fatal("conflict attachment succeeded")
	}
	rec, err := uc.repo.Recovery(tor.Hash)
	if err != nil || rec == nil || rec.Reason != models.StorageLegacyConflict {
		t.Fatalf("typed attachment reason missing: %+v %v", rec, err)
	}
	snapshot, err := uc.GetTorrent(tor.Hash)
	if err != nil || !snapshot.MetadataKnown || snapshot.StorageState != "ready" || snapshot.StorageRecovery.Runtime != "failed" {
		t.Fatalf("layout/runtime/metadata axes confused: %+v %v", snapshot, err)
	}
	return tor.Hash
}

func TestLegacyRecoveryAnalysisAndApply(t *testing.T) {
	for _, format := range []string{"v1", "v2", "hybrid"} {
		for _, equal := range []bool{true, false} {
			name := format + "/different"
			if equal {
				name = format + "/equal"
			}
			t.Run(name, func(t *testing.T) {
				uc, repo, cleanup := setupTorrentUseCase(t)
				defer cleanup()
				final := []byte("damaged!")
				if equal {
					final = []byte("original")
				}
				hash := legacyConflictFixture(t, uc, format, final, []byte("original"))
				before, _ := repo.Recovery(hash)
				if err := uc.analyzeLegacy(context.Background(), hash, before.RecordID, before.Version); err != nil {
					t.Fatal(err)
				}
				rec, _ := repo.Recovery(hash)
				j, err := decodeLegacyJournal(rec)
				if err != nil {
					t.Fatal(err)
				}
				choice := "part"
				if equal {
					choice = "final"
				}
				if j.Analysis == nil || j.Analysis.Recommended != choice || j.Analysis.Equal != equal {
					t.Fatalf("bad analysis: %+v", j.Analysis)
				}
				if rec.Version <= before.Version {
					t.Fatal("analysis did not advance operation version")
				}
				if err := uc.applyLegacy(context.Background(), hash, rec.RecordID, before.Version, choice); !errors.Is(err, ErrStorageConflict) {
					t.Fatal("stale analysis accepted", err)
				}
				if err := uc.applyLegacy(context.Background(), hash, rec.RecordID, rec.Version, choice); err != nil {
					t.Fatal(err)
				}
				rec, _ = repo.Recovery(hash)
				j, err = decodeLegacyJournal(rec)
				if err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(filepath.Join(uc.engine.DownloadDir(), payload.Root(hash), j.MetadataPath))
				if err != nil || string(got) != "original" {
					t.Fatalf("wrong selected payload: %q %v", got, err)
				}
				alt, err := os.ReadFile(filepath.Join(uc.engine.DownloadDir(), j.Backup))
				want := string(final)
				if equal {
					want = "original"
				}
				if err != nil || string(alt) != want {
					t.Fatalf("alternative lost: %q %v", alt, err)
				}
				saved, _ := repo.GetTorrent(hash)
				if saved.StorageRecovery.AlternativeCopies != 1 || saved.StorageRecovery.AlternativeBytes != 8 || saved.Control.DesiredRun != models.RunPaused {
					t.Fatalf("retention or paused intent missing: %+v", saved)
				}
				if uc.engine.GetRawTorrent(hash) != nil {
					t.Fatal("recovery auto-attached runtime")
				}
			})
		}
	}
}

func TestLegacyRecoveryRestartBoundaries(t *testing.T) {
	for _, boundary := range []string{"prepared", "alternative_moved", "selected_published", "verified"} {
		t.Run(boundary, func(t *testing.T) {
			uc, repo, cleanup := setupTorrentUseCase(t)
			defer cleanup()
			hash := legacyConflictFixture(t, uc, "v1", []byte("damaged!"), []byte("original"))
			rec, _ := repo.Recovery(hash)
			if err := uc.analyzeLegacy(context.Background(), hash, rec.RecordID, rec.Version); err != nil {
				t.Fatal(err)
			}
			rec, _ = repo.Recovery(hash)
			injected := errors.New("crash boundary")
			uc.recoveryStep = func(step string) error {
				if step == boundary {
					return injected
				}
				return nil
			}
			if err := uc.applyLegacy(context.Background(), hash, rec.RecordID, rec.Version, "part"); !errors.Is(err, injected) {
				t.Fatal("boundary not reached", err)
			}
			// Re-open a real SQLite pool and use a new usecase with no in-memory claim,
			// journal or callback. The stopped engine is shared; it has no runtime.
			db, err := repository.NewSQLiteDB(filepath.Join(uc.engine.DownloadDir(), "hub.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			restarted := NewTorrentUseCase(uc.engine, repository.NewTorrentRepo(db))
			if err := restarted.applyLegacy(context.Background(), hash, rec.RecordID, rec.Version, "part"); err != nil {
				t.Fatal(err)
			}
			if err := restarted.applyLegacy(context.Background(), hash, rec.RecordID, rec.Version, "part"); err != nil {
				t.Fatal("repeat created another backup", err)
			}
			saved, _ := repo.GetTorrent(hash)
			if saved.StorageRecovery.AlternativeCopies != 1 {
				t.Fatalf("duplicate backup: %+v", saved.StorageRecovery)
			}
		})
	}
}

func TestLegacyRecoveryRejectsChangedCandidate(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	hash := legacyConflictFixture(t, uc, "v1", []byte("damaged!"), []byte("original"))
	rec, _ := repo.Recovery(hash)
	if err := uc.analyzeLegacy(context.Background(), hash, rec.RecordID, rec.Version); err != nil {
		t.Fatal(err)
	}
	rec, _ = repo.Recovery(hash)
	j, _ := decodeLegacyJournal(rec)
	path := filepath.Join(uc.engine.DownloadDir(), payload.Root(hash), j.MetadataPath+".part")
	if err := os.WriteFile(path, []byte("changed!"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := uc.applyLegacy(context.Background(), hash, rec.RecordID, rec.Version, "part"); !errors.Is(err, ErrRecoveryCandidateChanged) {
		t.Fatalf("changed candidate accepted: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "changed!" {
		t.Fatal("candidate discarded", err)
	}
	uc.saveRecoveryFailure(hash, rec.RecordID, rec.Version, ErrRecoveryCandidateChanged)
	if err := uc.analyzeLegacy(context.Background(), hash, rec.RecordID, rec.Version); err != nil {
		t.Fatal("unchanged namespace could not be re-analyzed", err)
	}
}

func TestLegacyRecoveryRejectsLinkedCandidate(t *testing.T) {
	for _, kind := range []string{"hardlink", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			uc, repo, cleanup := setupTorrentUseCase(t)
			defer cleanup()
			hash := legacyConflictFixture(t, uc, "v1", []byte("damaged!"), []byte("original"))
			rec, _ := repo.Recovery(hash)
			if err := uc.analyzeLegacy(context.Background(), hash, rec.RecordID, rec.Version); err != nil {
				t.Fatal(err)
			}
			rec, _ = repo.Recovery(hash)
			j, _ := decodeLegacyJournal(rec)
			foreign := filepath.Join(uc.engine.DownloadDir(), "foreign")
			if err := os.WriteFile(foreign, []byte("foreign!"), 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(uc.engine.DownloadDir(), payload.Root(hash), j.MetadataPath+".part")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			link := os.Link
			if kind == "symlink" {
				link = os.Symlink
			}
			if err := link(foreign, path); err != nil {
				t.Skip(err)
			}
			err := uc.applyLegacy(context.Background(), hash, rec.RecordID, rec.Version, "part")
			if err == nil {
				t.Fatal("unsafe candidate moved")
			}
			uc.saveRecoveryFailure(hash, rec.RecordID, rec.Version, err)
			got, _ := repo.GetTorrent(hash)
			if got.StorageRecovery.FailureCode != "unsafe_candidate" {
				t.Fatalf("unsafe failure hidden %+v", got.StorageRecovery)
			}
			if content, err := os.ReadFile(foreign); err != nil || string(content) != "foreign!" {
				t.Fatal("foreign file changed", err)
			}
		})
	}
}
