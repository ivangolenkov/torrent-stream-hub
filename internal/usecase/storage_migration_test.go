package usecase

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

func migrationMetainfo(t *testing.T, name string, data []byte, multi bool, version string) *metainfo.MetaInfo {
	t.Helper()
	info := metainfo.Info{Name: name, PieceLength: 16384, Length: int64(len(data))}
	if multi {
		info.Length = 0
		info.Files = []metainfo.FileInfo{{Length: int64(len(data)), Path: []string{"video.bin"}}}
	}
	if version != "v2" {
		for off := 0; off < len(data); off += 16384 {
			h := sha1.Sum(data[off:min(off+16384, len(data))])
			info.Pieces = append(info.Pieces, h[:]...)
		}
	}
	if version == "v2" || version == "hybrid" {
		info.MetaVersion = 2
		info.Length = 0
		if version == "v2" {
			info.Files = nil
		}
		h := sha256.Sum256(data)
		info.FileTree = metainfo.FileTree{Dir: map[string]metainfo.FileTree{"video.bin": {File: metainfo.FileTreeFile{Length: int64(len(data)), PiecesRoot: string(h[:])}}}}
	}
	encoded, err := bencode.Marshal(&info)
	if err != nil {
		t.Fatal(err)
	}
	return &metainfo.MetaInfo{InfoBytes: encoded}
}

func legacyMigrationFixture(t *testing.T, uc *TorrentUseCase, mi *metainfo.MetaInfo, data []byte, part bool) (*models.Torrent, string) {
	t.Helper()
	spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		t.Fatal(err)
	}
	hash := spec.InfoHash.HexString()
	if spec.InfoHash == (metainfo.Hash{}) {
		h := spec.InfoHashV2.Value
		hash = h.ToShort().HexString()
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	paths, err := storagePaths(&info)
	if err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(uc.engine.ConfigDir(), "metainfo", hash+".torrent")
	if err := os.MkdirAll(filepath.Dir(meta), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := mi.Write(f); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	source := filepath.Join(uc.engine.DownloadDir(), paths[0])
	if part {
		source += ".part"
	}
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	tor := &models.Torrent{Hash: hash, Name: info.Name, Size: info.TotalLength(), Downloaded: info.TotalLength(), State: models.StatePaused, Files: []*models.File{{Index: 0, Path: info.UpvertedFiles()[0].DisplayPath(&info), Size: info.TotalLength(), Downloaded: info.TotalLength(), Priority: models.PriorityHigh}}}
	if err := uc.repo.SaveTorrentWithPolicy(tor, true); err != nil {
		t.Fatal(err)
	}
	if err := uc.repo.SaveStorageMapping(&repository.StorageMapping{Hash: hash, State: "legacy"}); err != nil {
		t.Fatal(err)
	}
	return tor, source
}

func waitMigrationReady(t *testing.T, uc *TorrentUseCase, hash string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ready, _, err := uc.CheckFileDirectReady(hash, 0)
		if err == nil && ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("verification did not finish: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStorageMigrationLegacyVersionsAndPriorities(t *testing.T) {
	for _, version := range []string{"v1", "v2", "hybrid"} {
		t.Run(version, func(t *testing.T) {
			uc, repo, cleanup := setupTorrentUseCase(t)
			defer cleanup()
			data := bytes.Repeat([]byte("verified"), 128)
			mi := migrationMetainfo(t, "legacy", data, true, version)
			tor, source := legacyMigrationFixture(t, uc, mi, data, false)
			queue, _ := repo.TemporaryQueue()
			if err := uc.migrateStorage(context.Background(), tor); err != nil {
				t.Fatal(err)
			}
			m, err := repo.StorageMapping(tor.Hash)
			if err != nil || m.State != "ready" || m.Version != 1 || m.Root != payload.Root(tor.Hash) || m.Manifest != "" {
				t.Fatalf("mapping=%+v err=%v", m, err)
			}
			if _, err := os.Stat(source); !os.IsNotExist(err) {
				t.Fatalf("legacy source not cleaned: %v", err)
			}
			waitMigrationReady(t, uc, tor.Hash)
			_, file, err := uc.GetDownloadedFile(tor.Hash, 0)
			if err != nil {
				t.Fatal(err)
			}
			in, err := file.Open()
			if err != nil {
				t.Fatal(err)
			}
			in.Close()
			stored, err := repo.GetTorrent(tor.Hash)
			if err != nil || !stored.IsTemporary || stored.State != models.StatePaused || stored.Files[0].Priority != models.PriorityHigh {
				t.Fatalf("policy changed: %+v %v", stored, err)
			}
			after, _ := repo.TemporaryQueue()
			if len(queue) != len(after) || after[0] != queue[0] {
				t.Fatal("FIFO changed")
			}
		})
	}
}

func TestStorageMigrationBlocksSharedAndOccupiedPaths(t *testing.T) {
	for _, mode := range []string{"shared", "occupied", "symlink", "hardlink", "no-metainfo", "partial", "bad-content", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			uc, repo, cleanup := setupTorrentUseCase(t)
			defer cleanup()
			data := bytes.Repeat([]byte("x"), 1024)
			mi := migrationMetainfo(t, "old", data, true, "v1")
			part := mode == "partial"
			sourceData := data
			if part {
				sourceData = data[:128]
			}
			if mode == "bad-content" {
				sourceData = bytes.Repeat([]byte("y"), len(data))
			}
			tor, source := legacyMigrationFixture(t, uc, mi, sourceData, part)
			if mode == "no-metainfo" {
				if err := os.Remove(filepath.Join(uc.engine.ConfigDir(), "metainfo", tor.Hash+".torrent")); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "hardlink" {
				if err := os.Link(source, filepath.Join(t.TempDir(), "alias")); err != nil {
					t.Skip(err)
				}
			}
			if mode == "shared" {
				other := migrationMetainfo(t, "old", bytes.Repeat([]byte("z"), 1024), true, "v1")
				legacyMigrationFixture(t, uc, other, sourceData, false)
			}
			if mode == "occupied" {
				if err := os.MkdirAll(filepath.Join(uc.engine.DownloadDir(), payload.Root(tor.Hash)), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "symlink" {
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.WriteFile(outside, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, source); err != nil {
					t.Skip(err)
				}
			}
			ctx := context.Background()
			if mode == "canceled" {
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			}
			err := uc.migrateStorage(ctx, tor)
			if mode == "partial" || mode == "bad-content" {
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := uc.GetDownloadedFile(tor.Hash, 0); err == nil {
					t.Fatal("unverified data admitted by historical 100% progress")
				}
			} else {
				if err == nil {
					t.Fatal("unsafe migration succeeded")
				}
				if _, err := os.Lstat(source); err != nil {
					t.Fatal("source lost", err)
				}
				m, _ := repo.StorageMapping(tor.Hash)
				if m.State == "ready" {
					t.Fatal("unsafe mapping activated")
				}
			}
		})
	}
}

func TestStorageMigrationCommitFailureAndRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DownloadDir: dir, DBPath: filepath.Join(dir, "hub.db"), TorrentPort: 0, BTDisableDHT: true, BTDisableUPNP: true}
	db, err := repository.NewSQLiteDB(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(cfg)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	repo := repository.NewTorrentRepo(db)
	uc := NewTorrentUseCase(eng, repo)
	data := bytes.Repeat([]byte("r"), 1024)
	tor, source := legacyMigrationFixture(t, uc, migrationMetainfo(t, "resume", data, true, "v1"), data, false)
	if _, err := db.DB().Exec(`CREATE TRIGGER fail_storage_commit BEFORE UPDATE OF version ON torrent_storage WHEN NEW.version=1 BEGIN SELECT RAISE(ABORT,'injected storage commit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := uc.migrateStorage(context.Background(), tor); err == nil {
		t.Fatal("commit failure ignored")
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal("source removed before mapping commit")
	}
	m, err := repo.StorageMapping(tor.Hash)
	if err != nil || m.State != "moving" || m.Manifest == "" {
		t.Fatalf("journal lost: %+v %v", m, err)
	}
	if err := uc.Delete(tor.Hash, true); !errors.Is(err, ErrStorageConflict) {
		t.Fatal("migration not protected", err)
	}
	if _, err := db.DB().Exec(`DROP TRIGGER fail_storage_commit`); err != nil {
		t.Fatal(err)
	}
	eng.Close()
	db.Close()
	db, err = repository.NewSQLiteDB(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	eng, err = engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	uc = NewTorrentUseCase(eng, repository.NewTorrentRepo(db))
	if err := uc.migrateStorage(context.Background(), tor); err != nil {
		t.Fatal(err)
	}
	waitMigrationReady(t, uc, tor.Hash)
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatal("source remains after resumed commit", err)
	}
}

func TestStorageCopyRecoversTemporaryPublication(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile("source", []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	digest, size, err := digestPayload(context.Background(), root, "source")
	if err != nil {
		t.Fatal(err)
	}
	e := migrationEntry{Source: "source", Target: "target", Size: size, Digest: digest}
	if err := root.WriteFile("target.tsh-migrate", []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := root.Link("target.tsh-migrate", "target"); err != nil {
		t.Fatal(err)
	}
	if err := copyMigrationEntry(context.Background(), root, &e); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Stat("target.tsh-migrate"); !os.IsNotExist(err) {
		t.Fatal("temporary alias survived")
	}
	if err := root.WriteFile("target", []byte("evil"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := copyMigrationEntry(context.Background(), root, &e); err == nil {
		t.Fatal("conflicting target overwritten")
	}
	if data, err := root.ReadFile("source"); err != nil || string(data) != "data" {
		t.Fatal("source lost")
	}
}

func TestStorageIsolationAndHybridAliases(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	add := func(data []byte, version string) *models.Torrent {
		mi := migrationMetainfo(t, "same-name", data, true, version)
		spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
		if err != nil {
			t.Fatal(err)
		}
		hash := spec.InfoHash.HexString()
		if spec.InfoHash == (metainfo.Hash{}) {
			v2 := spec.InfoHashV2.Value
			hash = v2.ToShort().HexString()
		}
		root := payload.Root(hash)
		if err := repo.SaveStorageMapping(&repository.StorageMapping{Hash: hash, Version: 1, Root: root, State: "ready"}); err != nil {
			t.Fatal(err)
		}
		info, _ := mi.UnmarshalInfo()
		paths, _ := storagePaths(&info)
		path := filepath.Join(uc.engine.DownloadDir(), root, paths[0])
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		var encoded bytes.Buffer
		if err := mi.Write(&encoded); err != nil {
			t.Fatal(err)
		}
		tor, err := uc.AddTorrentFile(&encoded)
		if err != nil {
			t.Fatal(err)
		}
		waitMigrationReady(t, uc, tor.Hash)
		return tor
	}
	a := add(bytes.Repeat([]byte("a"), 1024), "v1")
	b := add(bytes.Repeat([]byte("b"), 1024), "hybrid")
	if a.Hash == b.Hash {
		t.Fatal("fixtures have same hash")
	}
	mi, err := metainfo.LoadFromFile(filepath.Join(uc.engine.ConfigDir(), "metainfo", b.Hash+".torrent"))
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := torrent.TorrentSpecFromMetaInfoErr(mi)
	v2 := spec.InfoHashV2.Value
	aliasMagnet := metainfo.MagnetV2{}
	aliasMagnet.V2InfoHash.Set(v2)
	repeated, err := uc.AddMagnet(aliasMagnet.String())
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Hash != b.Hash {
		t.Fatal("hybrid alias created a second application identity")
	}
	all, err := repo.GetAllTorrents()
	if err != nil || len(all) != 2 {
		t.Fatalf("torrents=%d %v", len(all), err)
	}
	m, err := repo.StorageMapping(v2.ToShort().HexString())
	if err != nil || m.Root != payload.Root(b.Hash) {
		t.Fatalf("alias mapping %+v %v", m, err)
	}
	if err := uc.Delete(a.Hash, true); err != nil {
		t.Fatal(err)
	}
	_, file, err := uc.GetDownloadedFile(b.Hash, 0)
	if err != nil {
		t.Fatal(err)
	}
	f, err := file.Open()
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 1024)
	n, err := f.Read(data)
	f.Close()
	if err != nil || n != 1024 || !bytes.Equal(data, bytes.Repeat([]byte("b"), 1024)) {
		t.Fatal("deleting first torrent changed second payload")
	}
}

func TestHybridV2MagnetLearnsCanonicalAlias(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	mi := migrationMetainfo(t, "hybrid", bytes.Repeat([]byte("h"), 1024), true, "hybrid")
	spec, _ := torrent.TorrentSpecFromMetaInfoErr(mi)
	v2 := spec.InfoHashV2.Value
	magnet := metainfo.MagnetV2{}
	magnet.V2InfoHash.Set(v2)
	before, err := uc.AddMagnet(magnet.String())
	if err != nil {
		t.Fatal(err)
	}
	// Simulate metadata received directly by the library, not a second native add.
	raw := uc.engine.GetRawTorrent(before.Hash)
	if err := raw.SetInfoBytes(mi.InfoBytes); err != nil {
		t.Fatal(err)
	}
	canonical, err := repo.StorageMapping(spec.InfoHash.HexString())
	if err != nil {
		t.Fatal(err)
	}
	if canonical == nil || canonical.Root != payload.Root(before.Hash) {
		t.Fatalf("new root allocated for metadata alias: %+v", canonical)
	}
	var encoded bytes.Buffer
	mi.Write(&encoded)
	after, err := uc.AddTorrentFile(&encoded)
	if err != nil {
		t.Fatal(err)
	}
	if after.Hash != before.Hash {
		t.Fatalf("identity changed %s -> %s", before.Hash, after.Hash)
	}
	all, _ := repo.GetAllTorrents()
	if len(all) != 1 {
		t.Fatal("duplicate persisted owner")
	}
}

func TestStorageMigrationSpaceReserveRetainsSource(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DownloadDir: dir, DBPath: filepath.Join(dir, "hub.db"), MinFreeSpaceGB: 1_000_000_000, BTDisableDHT: true, BTDisableUPNP: true}
	db, err := repository.NewSQLiteDB(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	repo := repository.NewTorrentRepo(db)
	uc := NewTorrentUseCase(eng, repo)
	data := []byte("data")
	tor, source := legacyMigrationFixture(t, uc, migrationMetainfo(t, "space", data, false, "v1"), data, false)
	if err := uc.migrateStorage(context.Background(), tor); err == nil {
		t.Fatal("migration ignored free-space reserve")
	}
	got, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("source lost", err)
	}
	m, err := repo.StorageMapping(tor.Hash)
	if err != nil || m.State != "moving" || m.Manifest == "" {
		t.Fatalf("retry journal lost: %+v %v", m, err)
	}
}

func TestStorageCommittedJournalRevalidatesTargetBeforeCleanup(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	data := []byte("good")
	tor, source := legacyMigrationFixture(t, uc, migrationMetainfo(t, "committed", data, false, "v1"), data, false)
	root, err := os.OpenRoot(uc.engine.DownloadDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	rel, err := filepath.Rel(uc.engine.DownloadDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	digest, size, err := digestPayload(context.Background(), root, rel)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(payload.Root(tor.Hash), rel)
	if err := root.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(target, []byte("evil"), 0600); err != nil {
		t.Fatal(err)
	}
	j := migrationJournal{Root: payload.Root(tor.Hash), Entries: []migrationEntry{{Source: rel, Target: target, Digest: digest, Size: size, Copied: true}}}
	encoded, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveStorageMapping(&repository.StorageMapping{Hash: tor.Hash, Root: j.Root, Version: 1, State: "ready", Manifest: string(encoded)}); err != nil {
		t.Fatal(err)
	}
	if err := uc.migrateStorage(context.Background(), tor); err == nil {
		t.Fatal("changed target authorized cleanup")
	}
	got, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("source lost", err)
	}
	if err := uc.checkStorageAvailable(tor.Hash); err == nil {
		t.Fatal("pending journal admitted")
	}
}
