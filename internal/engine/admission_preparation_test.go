package engine

import (
	"bytes"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"torrent-stream-hub/internal/models"
)

// This probe is captured by value before admissionMu is released. Replacing the
// engine's probe while it is blocked cannot redirect the already-owned attempt.
func blockFirstProbe(t *testing.T, e *Engine) (<-chan struct{}, func()) {
	return blockProbeAfter(t, e, 0)
}

func blockProbeAfter(t *testing.T, e *Engine, skip int64) (<-chan struct{}, func()) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var blocked atomic.Bool
	var calls atomic.Int64
	var closeOnce sync.Once
	e.admissionMu.Lock()
	e.diskProbe = func(string) (uint64, error) {
		if calls.Add(1) > skip && blocked.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return 100 << 30, nil
	}
	e.admissionMu.Unlock()
	unblock := func() { closeOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	return entered, unblock
}

func TestAdmissionPreparationProbeDoesNotBlockIndependentPause(t *testing.T) {
	e, a := gateFixture(t)
	data := createTestTorrentBytes(4, "independent-probe", []metainfo.FileInfo{{Length: 4, Path: []string{"file"}}}, [][]byte{[]byte("data")})
	b, err := e.AddTorrentFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	entered, unblock := blockFirstProbe(t, e)
	finished := make(chan error, 1)
	go func() { finished <- e.CheckPayloadAdmission(a.hash) }()
	await(t, entered)
	paused := make(chan error, 1)
	go func() { paused <- e.Pause(b.Hash) }()
	select {
	case err := <-paused:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		unblock()
		t.Fatal("B waited for A's probe")
	}
	if err := e.Pause(a.hash); err != nil {
		t.Fatal(err)
	}
	unblock()
	if err := <-finished; !errors.Is(err, ErrOperationConflict) {
		t.Fatal("obsolete admission was accepted", err)
	}
	if a.t.DataDownloadAllowed() || e.GetTorrent(a.hash).State != models.StatePaused {
		t.Fatal("stale probe undid Pause")
	}
}

func TestAdmissionPreparationCannotAffectReaddedRuntime(t *testing.T) {
	e, a := gateFixture(t)
	entered, unblock := blockFirstProbe(t, e)
	finished := make(chan error, 1)
	go func() { finished <- e.CheckPayloadAdmission(a.hash) }()
	await(t, entered)
	if err := e.Delete(a.hash); err != nil {
		t.Fatal(err)
	}
	e.admissionMu.Lock()
	e.diskProbe = func(string) (uint64, error) { return 100 << 30, nil }
	e.admissionMu.Unlock()
	data := createTestTorrentBytes(4, "gate", []metainfo.FileInfo{{Length: 8, Path: []string{"video"}}}, [][]byte{[]byte("gooddata")})
	readded, err := e.AddTorrentFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	before := e.GetTorrent(readded.Hash)
	unblock()
	if err := <-finished; !errors.Is(err, ErrOperationConflict) {
		t.Fatal(err)
	}
	after := e.GetTorrent(readded.Hash)
	if after.State != before.State || after.DownloadBlock != "" || a.t.DataDownloadAllowed() {
		t.Fatal("stale prepare changed re-add", before, after)
	}
}

func TestSchedulerPreparationReservationCountsWhileProbeBlocked(t *testing.T) {
	e, a := gateFixture(t)
	if err := e.Pause(a.hash); err != nil {
		t.Fatal(err)
	}
	data := createTestTorrentBytes(4, "reserve-next", []metainfo.FileInfo{{Length: 4, Path: []string{"file"}}}, [][]byte{[]byte("data")})
	b, err := e.AddTorrentFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Pause(b.Hash); err != nil {
		t.Fatal(err)
	}
	e.admissionMu.Lock()
	a.mu.Lock()
	_, err = a.applyStateEventLocked(stateEvent{kind: stateActivated, phase: models.StateQueued})
	a.mu.Unlock()
	e.admissionMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// First probe is health, second is the reserved payload preparation.
	entered, unblock := blockProbeAfter(t, e, 1)
	finished := make(chan struct{})
	go func() { e.manageResources(); close(finished) }()
	await(t, entered)
	// A concurrent pass skips A's owned probe instead of waiting on its syscall.
	parallel := make(chan struct{})
	go func() { e.manageResources(); close(parallel) }()
	select {
	case <-parallel:
	case <-time.After(3 * time.Second):
		unblock()
		t.Fatal("parallel scheduler waited on owned probe")
	}
	e.admissionMu.Lock()
	count := e.slotCountLocked()
	e.admissionMu.Unlock()
	if count != 1 {
		unblock()
		t.Fatal("pending preparation lost reservation", count)
	}
	unblock()
	await(t, finished)
	if got := e.GetTorrent(a.hash); got.State != models.StateDownloading {
		t.Fatal("reservation not committed", got)
	}
}

func TestSchedulerPreparationStaleCleanupCannotReleaseNewReservation(t *testing.T) {
	e, a := gateFixture(t)
	if err := e.Pause(a.hash); err != nil {
		t.Fatal(err)
	}
	e.admissionMu.Lock()
	a.mu.Lock()
	_, err := a.applyStateEventLocked(stateEvent{kind: stateActivated, phase: models.StateQueued})
	a.mu.Unlock()
	e.admissionMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	entered, unblock := blockProbeAfter(t, e, 1)
	finished := make(chan struct{})
	go func() { e.manageResources(); close(finished) }()
	await(t, entered)
	a.mu.Lock()
	oldToken := a.slotToken
	a.mu.Unlock()
	if err := e.Pause(a.hash); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(a.hash); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	newToken, reserved := a.slotToken, a.policy.slot
	a.mu.Unlock()
	if !reserved || newToken == oldToken {
		unblock()
		t.Fatal("new reservation was not issued", oldToken, newToken, reserved)
	}
	unblock()
	await(t, finished)
	a.mu.Lock()
	reserved = a.policy.slot
	token := a.slotToken
	a.mu.Unlock()
	if !reserved || token != newToken {
		t.Fatal("old attempt released replacement reservation", oldToken, newToken, token)
	}
	e.manageResources()
	if !a.t.DataDownloadAllowed() {
		t.Fatal("fresh reservation could not admit after stale result retired")
	}
}

func TestAdmissionPreparationCloseJoinsProbeWithoutHoldingAdmission(t *testing.T) {
	e, a := gateFixture(t)
	entered, unblock := blockFirstProbe(t, e)
	prepared := make(chan error, 1)
	go func() { prepared <- e.CheckPayloadAdmission(a.hash) }()
	await(t, entered)
	closed := make(chan struct{})
	go func() { e.Close(); close(closed) }()
	await(t, e.ctx.Done())
	select {
	case <-closed:
		t.Fatal("Close discarded ownership of an in-flight probe")
	default:
	}
	// Shutdown must not wait for the probe while holding the lock needed by its
	// eventual validation/retirement path.
	lockAvailable := make(chan struct{})
	go func() { e.admissionMu.Lock(); e.admissionMu.Unlock(); close(lockAvailable) }()
	select {
	case <-lockAvailable:
	case <-time.After(3 * time.Second):
		unblock()
		t.Fatal("shutdown joined work under admissionMu")
	}
	unblock()
	if err := <-prepared; !errors.Is(err, ErrOperationConflict) {
		t.Fatal("shutdown accepted obsolete preparation", err)
	}
	await(t, closed)
	if a.t.DataDownloadAllowed() {
		t.Fatal("shutdown left payload open")
	}
}
