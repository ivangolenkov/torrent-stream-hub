package usecase

import (
	"bytes"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"path/filepath"
	"testing"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"
)

func priorityRestoreMetainfo(t *testing.T, name string) *metainfo.MetaInfo {
	t.Helper()
	mi := migrationMetainfo(t, name, []byte("gooddataxxxx"), true, "v1")
	info, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	info.Files = []metainfo.FileInfo{{Length: 4, Path: []string{"a"}}, {Length: 4, Path: []string{"b"}}, {Length: 4, Path: []string{"c"}}}
	mi.InfoBytes, err = bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	return mi
}

func TestPausedPrioritiesAndDiskLatchSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.db")
	open := func() (*TorrentUseCase, *repository.SQLiteDB) {
		t.Helper()
		db, err := repository.NewSQLiteDB(path)
		if err != nil {
			t.Fatal(err)
		}
		e, err := engine.New(&config.Config{DownloadDir: dir, DBPath: path, MaxActiveDownloads: 1, BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true})
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		return NewTorrentUseCase(e, repository.NewTorrentRepo(db)), db
	}
	uc, db := open()
	mi := priorityRestoreMetainfo(t, "priorities")
	var b bytes.Buffer
	if err := mi.Write(&b); err != nil {
		t.Fatal(err)
	}
	tor, err := uc.AddTorrentFile(bytes.NewReader(b.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(tor.Files) < 2 {
		t.Fatal("fixture requires multiple files")
	}
	if err := uc.SetFilePriority(tor.Hash, 0, models.PriorityHigh); err != nil {
		t.Fatal(err)
	}
	if err := uc.SetFilePriority(tor.Hash, 1, models.PriorityNone); err != nil {
		t.Fatal(err)
	}
	if err := uc.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	if err := uc.repo.SaveDownloadBlock(tor.Hash, "Disk full: persisted test latch"); err != nil {
		t.Fatal(err)
	}
	uc.engine.Close()
	db.Close()
	uc, db = open()
	defer db.Close()
	defer uc.engine.Close()
	if err := uc.RestoreTorrents(); err != nil {
		t.Fatal(err)
	}
	raw := uc.engine.GetRawTorrent(tor.Hash)
	if raw == nil || raw.DataDownloadAllowed() {
		t.Fatal("paused startup admitted payload")
	}
	NewSyncWorker(uc).syncOnce()
	saved, err := uc.repo.GetTorrent(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Files[0].Priority != models.PriorityHigh || saved.Files[1].Priority != models.PriorityNone || saved.Files[2].Priority != models.PriorityNormal || saved.DownloadBlock == "" {
		t.Fatalf("snapshot erased intent: %+v", saved)
	}
	if err := uc.Resume(tor.Hash); err != nil {
		t.Fatal(err)
	}
	if !raw.DataDownloadAllowed() {
		t.Fatal("manual Resume did not admit download")
	}
	if err := uc.Resume(tor.Hash); err != nil {
		t.Fatal("repeated Resume", err)
	}
	model := uc.engine.GetTorrent(tor.Hash)
	if model.Files[0].Priority != models.PriorityHigh || model.Files[1].Priority != models.PriorityNone || model.Files[2].Priority != models.PriorityNormal || model.DownloadBlock != "" {
		t.Fatal("Resume lost settings")
	}
}

func TestResumeDatabaseFailureKeepsAdmissionClosed(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	mi := priorityRestoreMetainfo(t, "db-fail")
	var b bytes.Buffer
	if err := mi.Write(&b); err != nil {
		t.Fatal(err)
	}
	tor, err := uc.AddTorrentFile(bytes.NewReader(b.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if err := uc.SetFilePriority(tor.Hash, 1, models.PriorityNone); err != nil {
		t.Fatal(err)
	}
	if err := uc.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	// A second connection installs an actual SQLite transaction failure, not a mock.
	db, err := repository.NewSQLiteDB(filepath.Join(uc.engine.DownloadDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB().Exec(`CREATE TRIGGER reject_resume BEFORE UPDATE ON torrents BEGIN SELECT RAISE(FAIL,'injected resume failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := uc.Resume(tor.Hash); err == nil {
		t.Fatal("Resume reported success on DB failure")
	}
	if uc.engine.GetRawTorrent(tor.Hash).DataDownloadAllowed() {
		t.Fatal("failed persistence admitted payload")
	}
	saved, err := repo.GetTorrent(tor.Hash)
	if err != nil || saved.Files[1].Priority != models.PriorityNone {
		t.Fatal("saved Skip lost", err)
	}
}

func TestDBOnlyMetadataPendingResumeAndSyncPreserveIntent(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	mi := priorityRestoreMetainfo(t, "pending")
	hash := mi.HashInfoBytes().HexString()
	saved := &models.Torrent{Hash: hash, State: models.StatePaused, SourceURI: "magnet:?xt=urn:btih:" + hash, Files: []*models.File{{Index: 0, Path: "pending/a", Size: 4, Priority: models.PriorityHigh}, {Index: 1, Path: "pending/b", Size: 4, Priority: models.PriorityNone}}}
	if err := repo.SaveTorrent(saved); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveStorageMapping(&repository.StorageMapping{Hash: hash, Version: payload.LayoutVersion, Root: payload.Root(hash), State: "ready"}); err != nil {
		t.Fatal(err)
	}
	if err := uc.Resume(hash); err != nil {
		t.Fatal(err)
	}
	raw := uc.engine.GetRawTorrent(hash)
	if raw.Info() != nil || raw.DataDownloadAllowed() {
		t.Fatal("metadata-pending payload admitted")
	}
	if err := uc.SetTorrentFilesPriority(hash, models.PriorityHigh); err != nil {
		t.Fatal(err)
	}
	if err := uc.SetFilePriority(hash, 1, models.PriorityNone); err != nil {
		t.Fatal(err)
	}
	NewSyncWorker(uc).syncOnce()
	saved, err := repo.GetTorrent(hash)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Files) != 2 || saved.Files[0].Priority != models.PriorityHigh || saved.Files[1].Priority != models.PriorityNone {
		t.Fatal("pending snapshot erased settings")
	}
	if err := raw.SetInfoBytes(mi.InfoBytes); err != nil {
		t.Fatal(err)
	}
	// A second Resume serializes with the metadata callback and must be idempotent.
	if err := uc.Resume(hash); err != nil {
		t.Fatal(err)
	}
	model := uc.engine.GetTorrent(hash)
	if model.Files[0].Priority != models.PriorityHigh || model.Files[1].Priority != models.PriorityNone {
		t.Fatal("metadata defaults overwrote saved intent")
	}
}

func TestActiveDownloadBlockRestoresWithoutAutomaticResume(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	mi := priorityRestoreMetainfo(t, "active-latch")
	var b bytes.Buffer
	if err := mi.Write(&b); err != nil {
		t.Fatal(err)
	}
	tor, err := uc.AddTorrentFile(bytes.NewReader(b.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if err := uc.SetFilePriority(tor.Hash, 1, models.PriorityNone); err != nil {
		t.Fatal(err)
	}
	if err := uc.engine.Delete(tor.Hash); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveDownloadBlock(tor.Hash, "Disk full: ENOSPC persisted"); err != nil {
		t.Fatal(err)
	}
	if err := uc.RestoreTorrents(); err != nil {
		t.Fatal(err)
	}
	model := uc.engine.GetTorrent(tor.Hash)
	if model.State != models.StateDiskFull || model.DownloadBlock == "" || uc.engine.GetRawTorrent(tor.Hash).DataDownloadAllowed() {
		t.Fatal("startup removed disk latch")
	}
	if _, err := uc.AddMagnet("magnet:?xt=urn:btih:" + tor.Hash); err == nil {
		t.Fatal("duplicate add bypassed explicit Resume")
	}
	if err := uc.Resume(tor.Hash); err != nil {
		t.Fatal(err)
	}
	if !uc.engine.GetRawTorrent(tor.Hash).DataDownloadAllowed() {
		t.Fatal("explicit Resume did not recover")
	}
}
