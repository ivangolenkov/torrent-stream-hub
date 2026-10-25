package usecase

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
)

func TestStorageProgressHeartbeatDoesNotInventIO(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	if err := repo.SaveTorrent(&models.Torrent{Hash: testInfoHash, State: models.StatePaused}); err != nil {
		t.Fatal(err)
	}
	tor, _ := repo.GetTorrent(testInfoHash)
	stop := uc.startStorageProgress(testInfoHash, tor.Control.RecordID, 1, models.StorageMigrationActive)
	defer stop()
	uc.storageStage(testInfoHash, "copy", int64ptr(8<<30), int64ptr(1))
	uc.storageBytes(testInfoHash, 128<<10)
	// Fake an old last-progress clock; no sleep or real multi-gigabyte I/O.
	uc.storageLiveMu.Lock()
	uc.storageLive[testInfoHash].progress.LastProgressAt = 1000
	uc.storageLiveMu.Unlock()
	for i := 0; i < 100; i++ {
		snapshot, _ := repo.GetTorrent(testInfoHash)
		uc.decorateStorage(snapshot)
		p := snapshot.StorageRecovery.Progress
		if p.BytesDone != 128<<10 || p.LastProgressAt != 1000 || p.HeartbeatAt <= 1000 {
			t.Fatalf("heartbeat invented progress: %+v", p)
		}
	}
	// An old record's live observation must never decorate a re-added hash.
	if err := repo.DeleteTorrent(testInfoHash); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveTorrent(&models.Torrent{Hash: testInfoHash, State: models.StatePaused}); err != nil {
		t.Fatal(err)
	}
	fresh, _ := repo.GetTorrent(testInfoHash)
	uc.decorateStorage(fresh)
	if fresh.StorageRecovery != nil {
		t.Fatal("old progress escaped into re-add")
	}
}

func TestMigrationObservedIOProfile(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	const smallFiles = 128
	info := metainfo.Info{Name: "profile", PieceLength: 16384}
	data := bytes.Repeat([]byte("s"), smallFiles)
	for i := 0; i < smallFiles; i++ {
		info.Files = append(info.Files, metainfo.FileInfo{Length: 1, Path: []string{fmt.Sprintf("small-%04d", i)}})
	}
	large := bytes.Repeat([]byte("L"), 1<<20)
	data = append(data, large...)
	info.Files = append(info.Files, metainfo.FileInfo{Length: int64(len(large)), Path: []string{"large"}})
	for off := 0; off < len(data); off += int(info.PieceLength) {
		sum := sha1.Sum(data[off:min(off+int(info.PieceLength), len(data))])
		info.Pieces = append(info.Pieces, sum[:]...)
	}
	encoded, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	tor, _ := legacyMigrationFixture(t, uc, &metainfo.MetaInfo{InfoBytes: encoded}, []byte("s"), false)
	tor.Files = nil
	for index, file := range info.Files {
		content := []byte("s")
		if index == smallFiles {
			content = large
		}
		if err := os.WriteFile(filepath.Join(uc.engine.DownloadDir(), info.Name, file.Path[0]), content, 0600); err != nil {
			t.Fatal(err)
		}
		tor.Files = append(tor.Files, &models.File{Index: index, Path: file.Path[0], Size: file.Length})
	}
	if err := repo.SaveTorrent(tor); err != nil {
		t.Fatal(err)
	}
	db, err := repository.NewSQLiteDB(filepath.Join(uc.engine.DownloadDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB().Exec(`CREATE TABLE profile_writes(n INTEGER);INSERT INTO profile_writes VALUES(0);CREATE TRIGGER count_profile AFTER UPDATE ON torrent_storage BEGIN UPDATE profile_writes SET n=n+1;END`); err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int64
	uc.storageByteHook = func(_ string, n int64) { reads.Add(n) }
	type metric struct {
		bytes    int64
		duration time.Duration
	}
	stages := make(map[string]metric)
	uc.storageStageHook = func(_ string, p models.StorageProgress, elapsed time.Duration) {
		v := stages[p.Stage]
		v.bytes += p.BytesDone
		v.duration += elapsed
		stages[p.Stage] = v
	}
	started := time.Now()
	if err := uc.migrateStorage(context.Background(), tor); err != nil {
		t.Fatal(err)
	}
	var writes int
	if err := db.DB().QueryRow(`SELECT n FROM profile_writes`).Scan(&writes); err != nil {
		t.Fatal(err)
	}
	if writes > 2*((len(info.Files)+63)/64)+3 {
		t.Fatalf("journal writes scale with chunks/files: %d", writes)
	}
	size := int64(len(data))
	if stages["checksum"].bytes != size || stages["copy"].bytes != 2*size || stages["cleanup"].bytes != 2*size {
		t.Fatalf("unexpected baseline physical reads: %+v", stages)
	}
	var stageBytes int64
	for _, v := range stages {
		stageBytes += v.bytes
	}
	if reads.Load() != stageBytes {
		t.Fatalf("observation lost/read extra bytes: total=%d stages=%d", reads.Load(), stageBytes)
	}
	t.Logf("local filesystem profile: files=%d payload=%d physical_read_bytes=%d journal_updates=%d elapsed=%s", len(info.Files), size, reads.Load(), writes, time.Since(started))
	for _, stage := range []string{"preflight", "checksum", "copy", "verification", "cleanup"} {
		v := stages[stage]
		t.Logf("stage=%s bytes=%d elapsed=%s", stage, v.bytes, v.duration)
	}
}
