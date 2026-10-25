package usecase

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/repository"

	"torrent-stream-hub/internal/models"
)

func TestStorageLocateBoundaryAndEmptyFile(t *testing.T) {
	for _, mode := range []string{"boundary", "missing-boundary", "empty", "missing-empty"} {
		t.Run(mode, func(t *testing.T) {
			uc, repo, done := setupTorrentUseCase(t)
			defer done()
			events := locationEvents(uc)
			info := metainfo.Info{Name: "source", PieceLength: 4}
			contents := map[string][]byte{"source": {}}
			if mode == "boundary" || mode == "missing-boundary" {
				first, last := sha1.Sum([]byte("abcd")), sha1.Sum([]byte("ef"))
				info.Files = []metainfo.FileInfo{{Length: 3, Path: []string{"a"}}, {Length: 3, Path: []string{"b"}}}
				info.Pieces = append(first[:], last[:]...)
				contents = map[string][]byte{"a": []byte("abc"), "b": []byte("def")}
			}
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
			dir := filepath.Join(uc.engine.DownloadDir(), "found")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			for name, data := range contents {
				if (mode == "missing-boundary" && name == "b") || mode == "missing-empty" {
					continue
				}
				if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			tor, _ = repo.GetTorrent(tor.Hash)
			m, _ := repo.StorageMapping(tor.Hash)
			cmd := models.StorageCommand{Action: "analyze_location", RecordID: tor.Control.RecordID, StorageID: m.StorageID, MappingRevision: m.Revision, Path: "found"}
			if len(info.Files) > 0 {
				cmd.StripPrefix = "source"
			}
			if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
				t.Fatal(err)
			}
			waitLocationEvent(t, uc, tor.Hash, events, "location_analyzed")
			after, _ := repo.GetTorrent(tor.Hash)
			a := after.Storage.Operation.Analysis
			wantMatch := mode == "boundary" || mode == "empty"
			if locationMatches(a) != wantMatch {
				t.Fatalf("unsafe candidate decision: %+v", a)
			}
			if mode == "missing-boundary" && a.ValidPieces != 0 {
				t.Fatal("trusted half of a shared piece")
			}
			if mode == "missing-empty" {
				if _, err := os.Stat(filepath.Join(dir, "source")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("analysis created empty file", err)
				}
			}
		})
	}
}

func TestStorageLocateRestartAfterMapping(t *testing.T) {
	uc, repo, closeFirst := setupTorrentUseCase(t)
	closed := false
	defer func() {
		if !closed {
			closeFirst()
		}
	}()
	tor, m := moveFixture(t, uc, false, []byte("located"))
	dir := uc.engine.DownloadDir()
	if err := os.Mkdir(filepath.Join(dir, "found"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "found", "video.bin"), []byte("located"), 0600); err != nil {
		t.Fatal(err)
	}
	events := make(chan string, 128)
	reached := make(chan struct{})
	uc.recoveryStep = func(stage string) error {
		if stage == "location_mapping_committed" {
			close(reached)
			<-uc.engine.Context().Done()
			return context.Canceled
		}
		select {
		case events <- stage:
		default:
		}
		return nil
	}
	cmd := models.StorageCommand{Action: "analyze_location", RecordID: tor.Control.RecordID, StorageID: m.StorageID, MappingRevision: m.Revision, Path: "found", StripPrefix: "Original"}
	if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
		t.Fatal(err)
	}
	waitLocationEvent(t, uc, tor.Hash, events, "location_analyzed")
	current, _ := repo.GetTorrent(tor.Hash)
	cmd.Action, cmd.Version = "apply_location", current.Storage.Operation.Version
	if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("commit not reached")
	}
	closeFirst()
	closed = true
	db, err := repository.NewSQLiteDB(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	eng, err := engine.New(&config.Config{DBPath: filepath.Join(dir, "hub.db"), DownloadDir: dir, BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true, BTDisableUTP: true, BTRetrackersMode: "off"})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	reopened := repository.NewTorrentRepo(db)
	_ = NewTorrentUseCase(eng, reopened)
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		rec, err := reopened.Recovery(tor.Hash)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Stage == "completed" {
			break
		}
		if rec.Error != "" {
			t.Fatal(rec.Error)
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("locate restart stalled")
		}
	}
	current, _ = reopened.GetTorrent(tor.Hash)
	if current.Storage.Revision != m.Revision+1 || current.Control.DesiredRun != models.RunPaused {
		t.Fatal("restart changed mapping or intent")
	}
	for _, path := range []string{filepath.Join(m.Root, "video.bin"), filepath.Join("found", "video.bin")} {
		data, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil || string(data) != "located" {
			t.Fatal("lost candidate/source", err)
		}
	}
}

func TestStorageLocateReadOnlyAndPausedApply(t *testing.T) {
	for _, format := range []string{"v1", "v2", "hybrid"} {
		t.Run(format, func(t *testing.T) {
			uc, repo, done := setupTorrentUseCase(t)
			defer done()
			events := locationEvents(uc)
			data := bytes.Repeat([]byte("known"), 5000)
			mi := migrationMetainfo(t, "Original", data, true, format)
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
			tor, _ = repo.GetTorrent(tor.Hash)
			m, _ := repo.StorageMapping(tor.Hash)
			dir := uc.engine.DownloadDir()
			for _, p := range []string{filepath.Join(m.Root, "video.bin"), "found/video.bin", "found/keep.part"} {
				full := filepath.Join(dir, p)
				if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			before, err := snapshotStorageTree(context.Background(), root, "found", nil)
			if err != nil {
				t.Fatal(err)
			}
			cmd := models.StorageCommand{Action: "analyze_location", RecordID: tor.Control.RecordID, StorageID: m.StorageID, MappingRevision: m.Revision, Path: "found", StripPrefix: "Original"}
			if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
				t.Fatal(err)
			}
			waitLocationEvent(t, uc, tor.Hash, events, "location_analyzed")
			if err := validateStorageTree(context.Background(), root, "found", before, nil); err != nil {
				t.Fatal("analysis wrote candidate", err)
			}
			current, _ := repo.GetTorrent(tor.Hash)
			a := current.Storage.Operation.Analysis
			if a == nil || a.FoundFiles != 1 || a.MissingFiles != 0 || a.ValidPieces != a.TotalPieces || a.ValidPieces == 0 {
				t.Fatalf("analysis %+v", a)
			}
			unchanged, _ := repo.StorageMapping(tor.Hash)
			if unchanged.Revision != m.Revision {
				t.Fatal("analysis applied mapping")
			}
			cmd.Action, cmd.Version = "apply_location", current.Storage.Operation.Version
			if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
				t.Fatal(err)
			}
			waitLocationEvent(t, uc, tor.Hash, events, "location_completed")
			current, _ = repo.GetTorrent(tor.Hash)
			if current.Storage.Root != "found" || current.Control.DesiredRun != models.RunPaused {
				t.Fatalf("not paused locate: %+v", current.Storage)
			}
			if b, err := os.ReadFile(filepath.Join(dir, m.Root, "video.bin")); err != nil || !bytes.Equal(b, data) {
				t.Fatal("source lost", err)
			}
			if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
				t.Fatal("replay", err)
			}
			ownership, _ := repo.ReadOwnership(context.Background())
			retained := false
			for _, c := range ownership.Claims {
				if c.Path == m.Root && c.Kind == "retained" {
					retained = true
				}
			}
			if !retained {
				t.Fatal("old location not protected")
			}
		})
	}
}

func TestStorageLocateWrongPartialAndChanged(t *testing.T) {
	for _, scenario := range []string{"wrong", "partial", "changed"} {
		t.Run(scenario, func(t *testing.T) {
			uc, repo, done := setupTorrentUseCase(t)
			defer done()
			events := locationEvents(uc)
			data := bytes.Repeat([]byte("x"), 32768)
			tor, m := moveFixture(t, uc, false, data)
			candidate := append([]byte{}, data...)
			if scenario == "wrong" {
				candidate = bytes.Repeat([]byte("z"), len(data))
			}
			if scenario == "partial" {
				candidate = candidate[:16384]
			}
			p := filepath.Join(uc.engine.DownloadDir(), "found", "video.bin")
			if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, candidate, 0600); err != nil {
				t.Fatal(err)
			}
			cmd := models.StorageCommand{Action: "analyze_location", RecordID: tor.Control.RecordID, StorageID: m.StorageID, MappingRevision: m.Revision, Path: "found", StripPrefix: "Original"}
			if _, err := uc.StorageCommand(context.Background(), tor.Hash, cmd); err != nil {
				t.Fatal(err)
			}
			waitLocationEvent(t, uc, tor.Hash, events, "location_analyzed")
			current, _ := repo.GetTorrent(tor.Hash)
			cmd.Action, cmd.Version = "apply_location", current.Storage.Operation.Version
			if scenario == "changed" {
				if err := os.WriteFile(p, []byte("replaced"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := uc.StorageCommand(context.Background(), tor.Hash, cmd)
			if scenario == "wrong" {
				if !errors.Is(err, ErrStorageConflict) {
					t.Fatal("wrong candidate accepted", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "partial" {
				if current.Storage.Operation.Analysis.ValidPieces != 1 {
					t.Fatal("partial evidence")
				}
				waitLocationEvent(t, uc, tor.Hash, events, "location_completed")
			} else {
				waitLocationEvent(t, uc, tor.Hash, events, "location_failed")
				v, _ := repo.Recovery(tor.Hash)
				if v.FailureCode != "candidate_changed" {
					t.Fatalf("failure %+v", v)
				}
				after, _ := repo.StorageMapping(tor.Hash)
				if after.Root != m.Root {
					t.Fatal("changed candidate applied")
				}
			}
		})
	}
}
