package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/models"
)

func gateFixture(t *testing.T) (*Engine, *ManagedTorrent) {
	t.Helper()
	e, m, _ := setupEngineWithTorrent(t, 4, "gate", []metainfo.FileInfo{{Length: 8, Path: []string{"video"}}}, [][]byte{[]byte("gooddata")})
	e.admissionMu.Lock()
	e.cfg.MaxActiveDownloads = 1
	e.diskProbe = func(string) (uint64, error) { return 100 << 30, nil }
	e.admissionMu.Unlock()
	e.manageResources()
	e.mu.RLock()
	mt := e.managedTorrents[m.Hash]
	e.mu.RUnlock()
	return e, mt
}

func TestDownloadGateLatchReadersAndResume(t *testing.T) {
	e, mt := gateFixture(t)
	streamCtx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()
	if err := e.streamManager.AddStream(streamCtx, mt.hash, 0, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := e.SetFilePriority(mt.hash, 0, models.PriorityHigh); err != nil {
		t.Fatal(err)
	}
	source, err := e.OpenStreamSource(context.Background(), mt.hash, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	read := make(chan error, 1)
	go func() { _, err := source.Reader.Read(make([]byte, 1)); read <- err }()
	var saved string
	e.SetDownloadBlockPersistence(func(_ string, reason string) error { saved = reason; return nil })
	e.admissionMu.Lock()
	e.diskProbe = func(string) (uint64, error) { return 0, nil }
	e.cfg.MinFreeSpaceGB = 1
	e.admissionMu.Unlock()
	e.manageResources()
	if mt.t.DataDownloadAllowed() {
		t.Fatal("disk gate admits payload")
	}
	if e.streamManager.ActiveStreamsForTorrent(mt.hash) != 1 {
		t.Fatal("DiskFull reset stream reference count")
	}
	select {
	case err := <-read:
		if !errors.Is(err, ErrDownloadBlocked) {
			t.Fatalf("reader cause: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader hung after DiskFull")
	}
	if err := e.streamManager.AddStream(context.Background(), mt.hash, 0, StreamOptions{}); !errors.Is(err, ErrDownloadBlocked) {
		t.Fatalf("stream bypass: %v", err)
	}
	if _, err := e.streamManager.AddPreload(context.Background(), mt.hash, 0, StreamOptions{}); !errors.Is(err, ErrDownloadBlocked) {
		t.Fatalf("preload bypass: %v", err)
	}
	if err := e.Resume(mt.hash); !errors.Is(err, ErrDownloadBlocked) {
		t.Fatalf("low-space resume: %v", err)
	}
	if saved == "" {
		t.Fatal("block not persisted")
	}
	e.admissionMu.Lock()
	e.diskProbe = func(string) (uint64, error) { return 1 << 30, nil }
	e.admissionMu.Unlock()
	e.manageResources()
	if mt.t.DataDownloadAllowed() {
		t.Fatal("free space automatically cleared latch")
	}
	if err := e.Resume(mt.hash); err != nil {
		t.Fatal(err)
	}
	if !mt.t.DataDownloadAllowed() || saved != "" {
		t.Fatal("successful boundary Resume did not clear latch")
	}
	if e.streamManager.ActiveStreamsForTorrent(mt.hash) != 1 {
		t.Fatal("Resume reset stream reference count")
	}
	if got := e.GetTorrent(mt.hash).Files[0].Priority; got != models.PriorityHigh {
		t.Fatalf("priority lost: %v", got)
	}
}

func TestDownloadGateProbeErrorQueuedAndPersistFailure(t *testing.T) {
	e, mt := gateFixture(t)
	if err := e.Pause(mt.hash); err != nil {
		t.Fatal(err)
	}
	e.SetDownloadBlockPersistence(func(string, string) error { return errors.New("database unavailable") })
	e.admissionMu.Lock()
	e.diskProbe = func(string) (uint64, error) { return 0, errors.New("statfs failed") }
	e.admissionMu.Unlock()
	e.manageResources()
	m := e.GetTorrent(mt.hash)
	if m.State != models.StatePaused || !strings.Contains(m.DownloadBlock, "unknown") {
		t.Fatalf("paused probe error: %+v", m)
	}
	if err := e.Resume(mt.hash); err == nil || errors.Is(err, ErrDownloadBlocked) {
		t.Fatalf("persist failure hidden: %v", err)
	}
	if mt.t.DataDownloadAllowed() {
		t.Fatal("persistence failure opened runtime gate")
	}
}

func TestDownloadGateConcurrentCallbacksNeverReopen(t *testing.T) {
	e, mt := gateFixture(t)
	e.payloadWriteError(mt, &os.PathError{Op: "write", Path: "payload", Err: syscall.ENOSPC})
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.manageResources()
			e.refreshPeerDiscovery(mt, "test", true)
			_ = e.CheckPayloadAdmission(mt.hash)
		}()
	}
	wg.Wait()
	if mt.t.DataDownloadAllowed() {
		t.Fatal("callback reopened ENOSPC latch")
	}
	if !strings.Contains(e.GetTorrent(mt.hash).DownloadBlock, "ENOSPC") {
		t.Fatal("writer diagnostic missing")
	}
	if err := e.Delete(mt.hash); err != nil {
		t.Fatal(err)
	}
	e.payloadWriteError(mt, syscall.ENOSPC) // Late observer cannot resurrect deleted state.
	if e.GetTorrent(mt.hash) != nil {
		t.Fatal("deleted torrent resurrected")
	}
}

func TestRestoredPrioritiesPrecedeMetadataAdmission(t *testing.T) {
	dir := t.TempDir()
	e, err := New(&config.Config{DownloadDir: dir, DBPath: filepath.Join(dir, "hub.db"), MaxActiveDownloads: 1, BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	data := createTestTorrentBytes(4, "restore", []metainfo.FileInfo{{Length: 4, Path: []string{"a"}}, {Length: 4, Path: []string{"b"}}}, [][]byte{[]byte("good"), []byte("data")})
	mi, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := torrent.TorrentSpecFromMagnetUri("magnet:?xt=urn:btih:" + mi.HashInfoBytes().HexString())
	if err != nil {
		t.Fatal(err)
	}
	saved := &models.Torrent{Hash: mi.HashInfoBytes().HexString(), State: models.StateDownloading, Files: []*models.File{{Index: 0, Priority: models.PriorityHigh}, {Index: 1, Priority: models.PriorityNone}}}
	if _, err := e.RestoreTorrent(spec, saved); err != nil {
		t.Fatal(err)
	}
	raw := e.GetRawTorrent(saved.Hash)
	e.manageResources()
	if raw.DataDownloadAllowed() {
		t.Fatal("staged restore admitted data")
	}
	if err := e.FinishRestore(saved.Hash, models.StateQueued); err != nil {
		t.Fatal(err)
	}
	if raw.DataDownloadAllowed() {
		t.Fatal("metadata-pending restore admitted data")
	}
	if err := raw.SetInfoBytes(mi.InfoBytes); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for !raw.DataDownloadAllowed() {
		select {
		case <-deadline:
			t.Fatal("metadata never admitted")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if raw.Files()[0].Priority() != torrent.PiecePriorityHigh || raw.Files()[1].Priority() != torrent.PiecePriorityNone {
		t.Fatal("wrong first-admission priorities")
	}
}

func TestDownloadGateVerifiedDirectReadSurvivesDiskFull(t *testing.T) {
	e, mt := gateFixture(t)
	root, err := e.StorageRoot(mt.hash)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.DownloadDir(), root, "gate", "video")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("gooddata"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := mt.t.VerifyData(); err != nil {
		t.Fatal(err)
	}
	e.payloadWriteError(mt, syscall.ENOSPC)
	src, err := e.OpenStreamSource(context.Background(), mt.hash, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if src.Type != StreamSourceDisk {
		t.Fatal("verified disk fast path blocked")
	}
	got, err := io.ReadAll(src.Reader)
	if err != nil || string(got) != "gooddata" {
		t.Fatalf("direct read: %q %v", got, err)
	}
}

func TestDownloadGatePersistenceBarrierClosesBeforeCommit(t *testing.T) {
	e, mt := gateFixture(t)
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	e.SetDownloadBlockPersistence(func(string, string) error { close(entered); <-release; return nil })
	go func() { defer close(finished); e.payloadWriteError(mt, syscall.ENOSPC) }()
	<-entered
	if mt.t.DataDownloadAllowed() {
		t.Fatal("payload remained enabled during persistence")
	}
	unblock()
	<-finished
	if mt.t.DataDownloadAllowed() {
		t.Fatal("persistence commit reopened gate")
	}
}

func TestCloseWithStagedMetadataRestore(t *testing.T) {
	e, mt := gateFixture(t)
	saved := e.GetTorrent(mt.hash)
	if err := e.StageExistingRestore(saved); err != nil {
		t.Fatal(err)
	}
	if mt.t.DataDownloadAllowed() {
		t.Fatal("staged restore is downloadable")
	}
	done := make(chan struct{})
	go func() { e.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close hung during restore")
	}
}

func TestQueuedTorrentDiskGateAndResumeSlotLimit(t *testing.T) {
	e, first := gateFixture(t)
	data := createTestTorrentBytes(4, "queued", []metainfo.FileInfo{{Length: 4, Path: []string{"second"}}}, [][]byte{[]byte("data")})
	second, err := e.AddTorrentFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if second.State != models.StateQueued || e.GetRawTorrent(second.Hash).DataDownloadAllowed() {
		t.Fatal("background limit bypassed")
	}
	if err := e.Resume(second.Hash); err != nil {
		t.Fatal(err)
	}
	if e.GetRawTorrent(second.Hash).DataDownloadAllowed() {
		t.Fatal("Resume bypassed active slot limit")
	}
	e.admissionMu.Lock()
	e.cfg.MinFreeSpaceGB = 1
	e.diskProbe = func(string) (uint64, error) { return 0, nil }
	e.admissionMu.Unlock()
	e.manageResources()
	for _, hash := range []string{first.hash, second.Hash} {
		if e.GetTorrent(hash).State != models.StateDiskFull || e.GetRawTorrent(hash).DataDownloadAllowed() {
			t.Fatalf("DiskFull missed queued/active %s", hash)
		}
	}
	e.admissionMu.Lock()
	e.diskProbe = func(string) (uint64, error) { return 100 << 30, nil }
	e.admissionMu.Unlock()
	e.manageResources()
	if err := e.Resume(second.Hash); err != nil {
		t.Fatal(err)
	}
	if !e.GetRawTorrent(second.Hash).DataDownloadAllowed() || first.t.DataDownloadAllowed() {
		t.Fatal("Resume cleared another torrent's latch")
	}
}

func TestDiskProbeChecksPayloadSubdirectories(t *testing.T) {
	e, mt := gateFixture(t)
	root, err := e.StorageRoot(mt.hash)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(e.DownloadDir(), root, "gate")
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	e.admissionMu.Lock()
	e.cfg.MinFreeSpaceGB = 1
	e.diskProbe = func(path string) (uint64, error) {
		if path == parent {
			return 0, nil
		}
		return 100 << 30, nil
	}
	e.admissionMu.Unlock()
	e.manageResources()
	if mt.t.DataDownloadAllowed() {
		t.Fatal("parent filesystem was not probed")
	}
}

func TestMetadataPersistenceFailureLeavesDownloadDisabled(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "config-file")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	e, err := New(&config.Config{DownloadDir: dir, DBPath: filepath.Join(blocked, "hub.db"), MaxActiveDownloads: 1, BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	data := createTestTorrentBytes(4, "bad-config", []metainfo.FileInfo{{Length: 4, Path: []string{"a"}}}, [][]byte{[]byte("good")})
	mi, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	hash := mi.HashInfoBytes().HexString()
	spec, err := torrent.TorrentSpecFromMagnetUri("magnet:?xt=urn:btih:" + hash)
	if err != nil {
		t.Fatal(err)
	}
	saved := &models.Torrent{Hash: hash, State: models.StateQueued, Files: []*models.File{{Index: 0, Priority: models.PriorityHigh}}}
	if _, err := e.RestoreTorrent(spec, saved); err != nil {
		t.Fatal(err)
	}
	if err := e.FinishRestore(hash, models.StateQueued); err != nil {
		t.Fatal(err)
	}
	raw := e.GetRawTorrent(hash)
	fence := models.RuntimeFence{IntentVersion: models.IntentVersion{Hash: hash, RecordID: "metadata-fault-record", Revision: 1}, Attachment: "metadata-fault-runtime"}
	if err := e.BindRuntime(raw, fence); err != nil {
		t.Fatal(err)
	}
	if err := raw.SetInfoBytes(mi.InfoBytes); err != nil {
		t.Fatal(err)
	}
	e.manageResources()
	deadline := time.After(2 * time.Second)
	for e.GetTorrent(hash).State != models.StateError {
		select {
		case <-deadline:
			t.Fatal("metadata persistence error not exposed")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if raw.DataDownloadAllowed() {
		t.Fatal("metadata persistence failure admitted data")
	}
	if model := e.GetTorrent(hash); model.Error != models.ErrMetadataPersistence || model.Control.AppliedRevision != 0 || raw.Files()[0].Priority() != torrent.PiecePriorityNone {
		t.Fatal("metadata fault retained acknowledgment/base requests", model)
	}
	if err := e.FinishRestore(hash, models.StateQueued, true); err == nil {
		t.Fatal("Resume ignored metadata persistence failure")
	}
	if e.GetTorrent(hash).Files[0].Priority != models.PriorityHigh {
		t.Fatal("initialization error lost priority")
	}
}
