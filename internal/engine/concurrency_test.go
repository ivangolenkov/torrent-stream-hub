package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"torrent-stream-hub/internal/models"
)

func await(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not terminate")
	}
}

func TestConcurrencyPauseRejectsLatePriorityApply(t *testing.T) {
	e, mt := gateFixture(t)
	sm := e.streamManager
	id, err := sm.addSession(mt.hash, 0, StreamOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := sm.refreshStreamOverlay(mt.hash, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.Pause(mt.hash); err != nil {
		t.Fatal(err)
	}
	// These are delayed operations created before Pause. None may restore High.
	var wg sync.WaitGroup
	for _, fn := range []func(){func() { _ = sm.refreshStreamOverlay(mt.hash, 0) }, func() { e.refreshPeerDiscovery(mt, "late", true) }, e.manageResources, func() { _ = e.SetFilePriority(mt.hash, 0, models.PriorityHigh) }, func() { e.finishRecheck(mt) }} {
		wg.Add(1)
		go func(fn func()) { defer wg.Done(); fn() }(fn)
	}
	wg.Wait()
	if mt.t.DataDownloadAllowed() || e.GetTorrent(mt.hash).State != models.StatePaused {
		t.Fatal("late callback undid Pause")
	}
	if mt.t.Files()[0].Priority() != torrent.PiecePriorityNone {
		t.Fatal("base priority restored on paused torrent")
	}
	sm.mu.Lock()
	ranges := len(sm.states[FileKey{Hash: mt.hash, Index: 0}].AppliedRanges)
	sm.mu.Unlock()
	if ranges != 0 {
		t.Fatal("paused overlay retained high ranges")
	}
	sm.removeSession(mt.hash, 0, id, false)
}

func TestConcurrencyStaleDebounceAndReleaseAfterReadd(t *testing.T) {
	e, mt := gateFixture(t)
	sm := e.streamManager
	id, err := sm.addSession(mt.hash, 0, StreamOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := sm.refreshStreamOverlay(mt.hash, 0); err != nil {
		t.Fatal(err)
	}
	sm.removeSession(mt.hash, 0, id, true)
	key := FileKey{Hash: mt.hash, Index: 0}
	sm.mu.Lock()
	old := sm.states[key]
	version := old.TimerVersion
	sm.mu.Unlock()
	newID, err := sm.addSession(mt.hash, 0, StreamOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := sm.refreshStreamOverlay(mt.hash, 0); err != nil {
		t.Fatal(err)
	}
	// Model an expired callback already queued when Stop loses the race.
	sm.expireDebounce(key, old, version)
	sm.mu.Lock()
	state := sm.states[key]
	count := state.ActiveStreams
	ranges := len(state.AppliedRanges)
	sm.mu.Unlock()
	if count != 1 || ranges == 0 {
		t.Fatal("expired debounce removed new session")
	}
	if err := e.Delete(mt.hash); err != nil {
		t.Fatal(err)
	}
	data := createTestTorrentBytes(4, "gate", []metainfo.FileInfo{{Length: 8, Path: []string{"video"}}}, [][]byte{[]byte("gooddata")})
	if _, err := e.AddTorrentFile(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	currentID, err := sm.addSession(mt.hash, 0, StreamOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if currentID == newID {
		t.Fatal("session token reused across generations")
	}
	e.finishRecheck(mt)
	sm.removeSession(mt.hash, 0, newID, false)
	sm.rollbackSession(mt.hash, 0, newID)
	sm.expireDebounce(key, old, version)
	sm.mu.Lock()
	state = sm.states[key]
	count = state.ActiveStreams
	sm.mu.Unlock()
	if count != 1 {
		t.Fatal("old cleanup affected replacement")
	}
	sm.removeSession(mt.hash, 0, currentID, false)
	sm.removeSession(mt.hash, 0, currentID, false)
}

func TestConcurrencySchedulerSlotReservations(t *testing.T) {
	for _, limit := range []int{1, 3} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			e, first := gateFixture(t)
			e.admissionMu.Lock()
			e.cfg.MaxActiveDownloads = limit
			e.admissionMu.Unlock()
			for i := 0; i < 6; i++ {
				data := createTestTorrentBytes(4, fmt.Sprintf("slot-%d", i), []metainfo.FileInfo{{Length: 4, Path: []string{"file"}}}, [][]byte{[]byte("data")})
				if _, err := e.AddTorrentFile(bytes.NewReader(data)); err != nil {
					t.Fatal(err)
				}
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			e.admissionMu.Lock()
			e.diskProbe = func(string) (uint64, error) {
				once.Do(func() { close(entered); <-release })
				active := 0
				e.mu.RLock()
				for _, mt := range e.managedTorrents {
					mt.mu.Lock()
					if mt.state == models.StateDownloading {
						active++
					}
					mt.mu.Unlock()
				}
				e.mu.RUnlock()
				if active > limit {
					t.Errorf("slot limit exceeded during admission: %d > %d", active, limit)
				}
				return 100 << 30, nil
			}
			e.admissionMu.Unlock()
			var wg sync.WaitGroup
			for range 12 {
				wg.Add(1)
				go func() { defer wg.Done(); e.manageResources() }()
			}
			await(t, entered)
			close(release)
			wg.Wait()
			active := 0
			for _, m := range e.GetAllTorrents() {
				if m.State == models.StateDownloading {
					active++
				}
			}
			if active != limit {
				t.Fatalf("active=%d limit=%d", active, limit)
			}
			if err := e.Pause(first.hash); err != nil {
				t.Fatal(err)
			}
			e.manageResources()
			active = 0
			for _, m := range e.GetAllTorrents() {
				if m.State == models.StateDownloading {
					active++
				}
			}
			if active != limit {
				t.Fatalf("released slot not reused: %d", active)
			}
		})
	}
}

func TestConcurrencyRecheckPauseAndCancellation(t *testing.T) {
	e, mt := gateFixture(t)
	entered, finish := make(chan struct{}), make(chan struct{})
	e.admissionMu.Lock()
	e.verifyTorrent = func(ctx context.Context, _ *torrent.Torrent) error {
		close(entered)
		select {
		case <-finish:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	e.admissionMu.Unlock()
	e.ScheduleRecheckIfProgressBehind(mt.hash, 8)
	await(t, entered)
	if err := e.Pause(mt.hash); err != nil {
		t.Fatal(err)
	}
	close(finish)
	// Close joins the recheck and all session watchers with uncancelled requests.
	closed := make(chan struct{})
	go func() { e.Close(); close(closed) }()
	await(t, closed)
	if mt.t.DataDownloadAllowed() {
		t.Fatal("recheck reopened closed generation")
	}
	started := false
	e.RunStorageTask(func(context.Context) { started = true })
	if started {
		t.Fatal("task accepted after Close")
	}
}

func TestConcurrencyDeleteCancelsRecheckAndMetadataWait(t *testing.T) {
	e, mt := gateFixture(t)
	entered, cancelled := make(chan struct{}), make(chan struct{})
	e.admissionMu.Lock()
	e.verifyTorrent = func(ctx context.Context, _ *torrent.Torrent) error {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	}
	e.admissionMu.Unlock()
	e.ScheduleRecheckIfProgressBehind(mt.hash, 8)
	await(t, entered)
	if err := e.Delete(mt.hash); err != nil {
		t.Fatal(err)
	}
	await(t, cancelled)
	m, err := e.AddMagnet("magnet:?xt=urn:btih:0123456789012345678901234567890123456789")
	if err != nil {
		t.Fatal(err)
	}
	e.ScheduleRecheckIfProgressBehind(m.Hash, 1)
	if err := e.Delete(m.Hash); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() { e.Close(); close(closed) }()
	await(t, closed)
}

func TestConcurrencyOverlayUnionAndConcurrentRefresh(t *testing.T) {
	e, m, _ := setupEngineWithTorrent(t, 4, "shared", []metainfo.FileInfo{{Length: 6, Path: []string{"a"}}, {Length: 6, Path: []string{"b"}}}, [][]byte{[]byte("abcdef"), []byte("ghijkl")})
	sm := e.streamManager
	if err := e.SetTorrentFilesPriority(m.Hash, models.PriorityNone); err != nil {
		t.Fatal(err)
	}
	e.admissionMu.Lock()
	e.cfg.MaxActiveDownloads = 1
	e.admissionMu.Unlock()
	e.manageResources()
	a, err := sm.addSession(m.Hash, 0, StreamOptions{HasRange: true, RangeStart: 5, WindowBytes: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := sm.addSession(m.Hash, 1, StreamOptions{WindowBytes: 1}, true)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := sm.refreshStreamOverlay(m.Hash, i%2); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	raw := e.GetRawTorrent(m.Hash)
	if raw.Piece(1).State().Priority != torrent.PiecePriorityHigh {
		t.Fatal("shared boundary piece not protected")
	}
	sm.removeSession(m.Hash, 0, a, false)
	if raw.Piece(1).State().Priority != torrent.PiecePriorityHigh {
		t.Fatal("clearing first file removed second file's overlay")
	}
	sm.removeSession(m.Hash, 1, b, false)
	if raw.Piece(1).State().Priority != torrent.PiecePriorityNone {
		t.Fatal("last release left stale High priority")
	}
	for _, file := range e.GetTorrent(m.Hash).Files {
		if file.Priority != models.PriorityNone {
			t.Fatal("overlay rewrote saved base selection")
		}
	}
}

func TestConcurrencyCloseJoinsUncancelledStreamAndPreload(t *testing.T) {
	e, mt := gateFixture(t)
	if err := e.streamManager.AddStream(context.Background(), mt.hash, 0, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	release, err := e.streamManager.AddPreload(context.Background(), mt.hash, 0, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() { e.Close(); close(closed) }()
	await(t, closed)
	release()
	release()
	if e.streamManager.ActiveStreamsTotal() != 0 {
		t.Fatal("Close retained stream state")
	}
	if _, err := e.AddMagnet("magnet:?xt=urn:btih:" + mt.hash); err == nil {
		t.Fatal("add admitted after Close")
	}
}

func TestConcurrencyFailedAdmissionReleasesSlot(t *testing.T) {
	e, first := gateFixture(t)
	data := createTestTorrentBytes(4, "next-slot", []metainfo.FileInfo{{Length: 4, Path: []string{"file"}}}, [][]byte{[]byte("data")})
	second, err := e.AddTorrentFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	root, err := e.StorageRoot(first.hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.DownloadDir(), root), 0700); err != nil {
		t.Fatal(err)
	}
	// Payload parent already exists after the fixture's verification pass.
	e.admissionMu.Lock()
	e.cfg.MinFreeSpaceGB = 1
	e.diskProbe = func(path string) (uint64, error) {
		if strings.Contains(path, root) {
			return 0, nil
		}
		return 100 << 30, nil
	}
	e.admissionMu.Unlock()
	e.manageResources()
	if e.GetTorrent(first.hash).State != models.StateDiskFull || e.GetTorrent(second.Hash).State != models.StateDownloading {
		t.Fatal("failed admission retained background slot")
	}
}

func TestConcurrencyCloseTaskAdmissionBarrier(t *testing.T) {
	e, _ := gateFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	e.RunStorageTask(func(ctx context.Context) { close(entered); <-ctx.Done(); <-release })
	await(t, entered)
	closed := make(chan struct{})
	go func() { e.Close(); close(closed) }()
	await(t, e.ctx.Done())
	if e.beginWork() {
		e.wg.Done()
		t.Fatal("task admitted after shutdown cancellation")
	}
	select {
	case <-closed:
		t.Fatal("Close did not join accepted task")
	default:
	}
	unblock()
	await(t, closed)
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() { defer wg.Done(); e.Close() }()
	}
	wg.Wait()
}
