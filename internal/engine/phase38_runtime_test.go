package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"torrent-stream-hub/internal/models"
)

func TestSamplerUnaffectedByAPIReads(t *testing.T) {
	e, mt := gateFixture(t)
	base := time.Now().Add(time.Hour)
	stats := torrent.TorrentStats{}
	stats.BytesReadUsefulData.Add(100)
	e.admissionMu.Lock()
	mt.mu.Lock()
	e.sampleSpeedsLocked(mt, stats, base)
	mt.mu.Unlock()
	e.admissionMu.Unlock()
	for range 100 {
		e.GetTorrent(mt.hash)
		e.GetAllTorrents()
		e.BTHealth()
	}
	mt.mu.Lock()
	if mt.lastStatsAt != base || mt.lastReadBytes != 100 {
		t.Fatal("API changed sampling baseline")
	}
	stats.BytesReadUsefulData.Add(200)
	e.sampleSpeedsLocked(mt, stats, base.Add(2*time.Second))
	if mt.downloadSpeed != 100 {
		t.Fatalf("speed=%d", mt.downloadSpeed)
	}
	e.sampleSpeedsLocked(mt, stats, base.Add(time.Second))
	if mt.downloadSpeed != 100 {
		t.Fatal("nonpositive interval changed sample")
	}
	e.sampleSpeedsLocked(mt, torrent.TorrentStats{}, base.Add(3*time.Second))
	if mt.downloadSpeed != 0 {
		t.Fatal("counter reset yielded negative speed")
	}
	mt.stallStartedAt = time.Now().Add(-time.Hour)
	mt.mu.Unlock()
	e.checkSwarms() // Future/stale samples must not trigger a false low-speed refresh.
}

func TestRecheckSingleflightPauseAndFailure(t *testing.T) {
	e, mt := gateFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	e.admissionMu.Lock()
	e.verifyTorrent = func(ctx context.Context, _ *torrent.Torrent) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	e.admissionMu.Unlock()
	op, err := e.StartRecheck(mt.hash)
	if err != nil {
		t.Fatal(err)
	}
	await(t, entered)
	second, err := e.StartRecheck(mt.hash)
	if err != nil || second.ID != op.ID {
		t.Fatal("duplicate verifier admitted", err)
	}
	if ready, _, err := e.CheckFileDirectReady(mt.hash, 0); err != nil || ready {
		t.Fatal("readiness exposed during recheck", err)
	}
	if model := e.GetTorrent(mt.hash); model.Recheck == nil || model.ProgressSource != "checking" {
		t.Fatal("operation not observable")
	}
	if err := e.Pause(mt.hash); err != nil {
		t.Fatal(err)
	}
	close(release)
	// Join the task without a sleep or consuming the engine's global WaitGroup.
	deadline := time.After(3 * time.Second)
	for {
		model := e.GetTorrent(mt.hash)
		if model.Recheck.Status == "succeeded" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("recheck did not complete")
		default:
		}
		time.Sleep(time.Millisecond)
	}
	if mt.t.DataDownloadAllowed() || e.GetTorrent(mt.hash).State != models.StatePaused {
		t.Fatal("recheck undid Pause")
	}
	e.admissionMu.Lock()
	e.verifyTorrent = func(context.Context, *torrent.Torrent) error { return errors.New("injected verifier failure") }
	e.admissionMu.Unlock()
	if _, err := e.StartRecheck(mt.hash); err != nil {
		t.Fatal(err)
	}
	deadline = time.After(3 * time.Second)
	for {
		model := e.GetTorrent(mt.hash)
		if model.Recheck.Status == "failed" {
			if model.Recheck.Error == "" {
				t.Fatal("missing error")
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("failed recheck did not finish")
		default:
		}
		time.Sleep(time.Millisecond)
	}
	if mt.t.DataDownloadAllowed() || e.GetTorrent(mt.hash).State != models.StatePaused {
		t.Fatal("failure opened payload gate")
	}
}

func TestSelectionCompleteDoesNotHoldSlotOrClaimFullSeeding(t *testing.T) {
	e, mt := gateFixture(t)
	if err := e.SetTorrentFilesPriority(mt.hash, models.PriorityNone); err != nil {
		t.Fatal(err)
	}
	model := e.GetTorrent(mt.hash)
	if !model.SelectionComplete || model.State != models.StateQueued || mt.t.DataDownloadAllowed() {
		t.Fatal("Skip All retained a slot", model)
	}
	if err := e.SetFilePriority(mt.hash, 0, models.PriorityNormal); err != nil {
		t.Fatal(err)
	}
	model = e.GetTorrent(mt.hash)
	if model.SelectionComplete || model.State != models.StateDownloading {
		t.Fatal("new selection did not rejoin background queue", model)
	}
}

func TestCacheCompletedUsesFakeLimit(t *testing.T) {
	e, tor := setupCompletedLifecycleTorrent(t)
	e.admissionMu.Lock()
	e.cfg.StreamCacheSize = 128
	e.admissionMu.Unlock()
	status, err := e.GetCacheStatus(tor.Hash, 0, 0)
	if err != nil || status.VirtualCacheBytes != 128 || !status.IsFullyDownloaded {
		t.Fatal("complete path bypassed limit", status, err)
	}
	status, err = e.GetCacheStatus(tor.Hash, 0, 2040)
	if err != nil || status.VirtualCacheBytes != 8 {
		t.Fatal("exceeded remaining bytes", status, err)
	}
	e.cfg.StreamCacheSize = 0
	status, err = e.GetCacheStatus(tor.Hash, 0, -1)
	if err != nil || status.VirtualCacheBytes != 0 {
		t.Fatal("zero limit bypassed", status, err)
	}
}

func TestFailedRecheckDoesNotRestoreOldReadiness(t *testing.T) {
	e, tor := setupCompletedLifecycleTorrent(t)
	if err := e.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	e.admissionMu.Lock()
	e.verifyTorrent = func(context.Context, *torrent.Torrent) error { return errors.New("read failure") }
	e.admissionMu.Unlock()
	if _, err := e.StartRecheck(tor.Hash); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for {
		if e.GetTorrent(tor.Hash).Recheck.Status == "failed" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("operation did not finish")
		default:
		}
		time.Sleep(time.Millisecond)
	}
	if ready, _, err := e.CheckFileDirectReady(tor.Hash, 0); err != nil || ready {
		t.Fatal("failed verification restored old readiness", err)
	}
	if err := e.Resume(tor.Hash); !errors.Is(err, ErrOperationConflict) {
		t.Fatal("Resume bypassed failed verification", err)
	}
}

func TestPartialSelectedCompletionDoesNotBecomeSeeding(t *testing.T) {
	e, tor, root := setupEngineWithTorrent(t, 4, "selection", []metainfo.FileInfo{{Length: 4, Path: []string{"a"}}, {Length: 4, Path: []string{"b"}}}, [][]byte{[]byte("data"), []byte("more")})
	path := filepath.Join(root, "selection", "a")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	verifyEngineTorrentPieces(t, e, tor.Hash, 0, 1)
	if err := e.SetFilePriority(tor.Hash, 1, models.PriorityNone); err != nil {
		t.Fatal(err)
	}
	model := e.GetTorrent(tor.Hash)
	if !model.SelectionComplete || model.State != models.StateQueued || model.Progress != 50 {
		t.Fatalf("partial selection mistaken for payload completion: %+v", model)
	}
}

func TestRecheckCancelledByClose(t *testing.T) {
	e, mt := gateFixture(t)
	entered := make(chan struct{})
	e.admissionMu.Lock()
	e.verifyTorrent = func(ctx context.Context, _ *torrent.Torrent) error { close(entered); <-ctx.Done(); return ctx.Err() }
	e.admissionMu.Unlock()
	if _, err := e.StartRecheck(mt.hash); err != nil {
		t.Fatal(err)
	}
	await(t, entered)
	e.Close()
	mt.mu.Lock()
	status := mt.recheckOperation.Status
	mt.mu.Unlock()
	if status != "cancelled" {
		t.Fatalf("Close did not cancel operation: %s", status)
	}
}
