package engine

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/models"
)

func TestStateTransitionSlotAdmissionMatrix(t *testing.T) {
	phases := []models.TorrentState{models.StateQueued, models.StateDownloading, models.StateSeeding, models.StatePaused, models.StateDiskFull, models.StateError, models.StateMissingFiles, models.StateStreaming}
	for _, phase := range phases {
		for flags := 0; flags < 64; flags++ {
			f := stateFacts{verificationFacts: verificationFacts{state: phase, initializing: flags&1 != 0, checking: flags&2 != 0, uncertain: flags&4 != 0}, selectionComplete: flags&8 != 0}
			if flags&16 != 0 {
				f.diskBlock = "sticky"
			}
			available := flags&32 != 0
			d, err := stateTransition(f, stateEvent{kind: stateSlotRequested, slotAvailable: available})
			want := phase == models.StateQueued && flags == 32
			if !want {
				var typed StateTransitionError
				if !errors.As(err, &typed) || !errors.Is(err, ErrOperationConflict) || d != (stateDecision{}) {
					t.Fatalf("phase=%s flags=%d: rejected event has effects: %+v %v", phase, flags, d, err)
				}
				continue
			}
			if err != nil || d.next.state != models.StateDownloading || d.next.diskBlock != f.diskBlock || d.next.checking || d.next.uncertain || d.closePayload || d.allowUpload || d.clearBasePriorities {
				t.Fatalf("bad grant: %+v %v", d, err)
			}
		}
	}
}

func TestStateTransitionSelectionMatrix(t *testing.T) {
	phases := []models.TorrentState{models.StateQueued, models.StateDownloading, models.StateSeeding, models.StatePaused, models.StateDiskFull, models.StateError, models.StateMissingFiles}
	for _, phase := range phases {
		for flags := 0; flags < 16; flags++ {
			for _, completion := range []struct{ selected, whole bool }{{false, false}, {true, false}, {true, true}} {
				f := stateFacts{verificationFacts: verificationFacts{state: phase, err: "original diagnostic", initializing: flags&1 != 0, checking: flags&2 != 0, uncertain: flags&4 != 0}}
				if flags&8 != 0 {
					f.diskBlock = "sticky"
				}
				d, err := stateTransition(f, stateEvent{kind: stateSelectionObserved, metadataKnown: true, selectedComplete: completion.selected, payloadComplete: completion.whole})
				if flags&7 != 0 {
					if !errors.Is(err, ErrOperationConflict) || d != (stateDecision{}) {
						t.Fatal("completion ignored veto", d, err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				eligible := phase == models.StateQueued || phase == models.StateDownloading || phase == models.StateSeeding
				want, reason := phase, f.err
				if eligible && f.diskBlock == "" {
					if completion.whole {
						want, reason = models.StateSeeding, models.ErrNone
					} else if completion.selected || phase == models.StateSeeding {
						want, reason = models.StateQueued, models.ErrNone
					}
				}
				if d.next.state != want || d.next.err != reason || d.next.diskBlock != f.diskBlock || d.selectionComplete != completion.selected {
					t.Fatalf("phase=%s flags=%d completion=%+v decision=%+v", phase, flags, completion, d)
				}
				cleared := want == models.StateQueued && phase != models.StateQueued
				if d.clearBasePriorities != cleared || d.closeBackgroundPayload != cleared {
					t.Fatal("wrong slot release effects", d)
				}
			}
		}
	}
	for _, event := range []stateEvent{{kind: "unknown"}, {kind: stateSelectionObserved}, {kind: stateSelectionObserved, metadataKnown: true, payloadComplete: true}} {
		if d, err := stateTransition(stateFacts{}, event); !errors.Is(err, ErrOperationConflict) || d != (stateDecision{}) {
			t.Fatal("invalid proof/event accepted", d, err)
		}
	}
}

func TestStateTransitionReleasePreservesOtherPhases(t *testing.T) {
	for _, phase := range []models.TorrentState{models.StateQueued, models.StateDownloading, models.StateSeeding, models.StatePaused, models.StateDiskFull, models.StateError, models.StateMissingFiles} {
		d, err := stateTransition(stateFacts{verificationFacts: verificationFacts{state: phase}}, stateEvent{kind: stateSlotReleased})
		if phase != models.StateDownloading {
			if !errors.Is(err, ErrOperationConflict) || d != (stateDecision{}) {
				t.Fatal("unowned slot cleanup has effects", phase, d, err)
			}
		} else if err != nil || d.next.state != models.StateQueued || !d.resetDownload || !d.clearBasePriorities || !d.closeBackgroundPayload {
			t.Fatal(d, err)
		}
	}
}

func TestStateTransitionRejectedExecutorDoesNotMutateRuntime(t *testing.T) {
	e, mt := gateFixture(t)
	e.admissionMu.Lock()
	mt.mu.Lock()
	state, reason, selected, started := mt.state, mt.err, mt.selectionComplete, mt.downloadAllStarted
	_, err := mt.applyStateEventLocked(stateEvent{kind: stateSlotRequested, slotAvailable: false})
	unchanged := mt.state == state && mt.err == reason && mt.selectionComplete == selected && mt.downloadAllStarted == started
	mt.mu.Unlock()
	e.admissionMu.Unlock()
	if !errors.Is(err, ErrOperationConflict) || !unchanged {
		t.Fatal("rejected executor mutated runtime", err)
	}
}

func TestStateTransitionSkipAllHandsSlotToNextFIFO(t *testing.T) {
	e, first := gateFixture(t)
	data := createTestTorrentBytes(4, "handoff", []metainfo.FileInfo{{Length: 4, Path: []string{"next"}}}, [][]byte{[]byte("data")})
	second, err := e.AddTorrentFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	e.SetQueuePosition(first.hash, 1)
	e.SetQueuePosition(second.Hash, 2)
	if second.State != models.StateQueued {
		t.Fatal("fixture bypassed limit", second)
	}
	if err := e.SetTorrentFilesPriority(first.hash, models.PriorityNone); err != nil {
		t.Fatal(err)
	}
	before, after := e.GetTorrent(first.hash), e.GetTorrent(second.Hash)
	if before.State != models.StateQueued || !before.SelectionComplete || before.Progress != 0 || first.t.DataDownloadAllowed() {
		t.Fatal("selected-only completion retained slot/readiness", before)
	}
	if first.t.Files()[0].Priority() != torrent.PiecePriorityNone {
		t.Fatal("base priority retained after release")
	}
	if after.State != models.StateDownloading || !e.GetRawTorrent(second.Hash).DataDownloadAllowed() {
		t.Fatal("released slot not reused in FIFO order", after)
	}
	// A cleanup which arrives after Pause must not restore Queued or open gates.
	if err := e.Pause(second.Hash); err != nil {
		t.Fatal(err)
	}
	e.mu.RLock()
	next := e.managedTorrents[second.Hash]
	e.mu.RUnlock()
	e.admissionMu.Lock()
	e.releaseFailedSlotLocked(next)
	e.admissionMu.Unlock()
	if got := e.GetTorrent(second.Hash); got.State != models.StatePaused || e.GetRawTorrent(second.Hash).DataDownloadAllowed() {
		t.Fatal("late cleanup undid Pause", got)
	}
}

func TestStateTransitionFailedAdmissionCleanupPreservesDiskLatch(t *testing.T) {
	e, mt := gateFixture(t)
	e.admissionMu.Lock()
	err := e.blockDownloadLocked(mt, "sticky")
	e.releaseFailedSlotLocked(mt)
	e.admissionMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got := e.GetTorrent(mt.hash); got.State != models.StateDiskFull || got.DownloadBlock != "sticky" || mt.t.DataDownloadAllowed() {
		t.Fatal("cleanup erased admission failure", got)
	}
}

func TestStateTransitionDiskPolicyKeepsIndependentAxes(t *testing.T) {
	for _, phase := range []models.TorrentState{models.StateQueued, models.StateDownloading, models.StateSeeding, models.StatePaused, models.StateError, models.StateMissingFiles} {
		f := stateFacts{verificationFacts: verificationFacts{state: phase, checking: true, uncertain: true, initializing: true}, selectionComplete: true}
		d, err := stateTransition(f, stateEvent{kind: stateDiskBlocked, diskReason: "sticky"})
		want := models.StateDiskFull
		if phase == models.StatePaused {
			want = models.StatePaused
		}
		if err != nil || d.next.state != want || d.next.diskBlock != "sticky" || !d.next.checking || !d.next.uncertain || !d.next.initializing || !d.selectionComplete || !d.closePayload || !d.resetDownload || d.closeUpload {
			t.Fatal("disk event lost an independent axis", d, err)
		}
	}
	if d, err := stateTransition(stateFacts{}, stateEvent{kind: stateDiskBlocked}); !errors.Is(err, ErrOperationConflict) || d != (stateDecision{}) {
		t.Fatal("invalid disk event has effects", d, err)
	}
}

func TestStateTransitionSkipAllKeepsIndependentStreamOverlay(t *testing.T) {
	e, mt := gateFixture(t)
	sm := e.streamManager
	id, err := sm.addSession(mt.hash, 0, StreamOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer sm.removeSession(mt.hash, 0, id, false)
	if err := sm.refreshStreamOverlay(mt.hash, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.SetTorrentFilesPriority(mt.hash, models.PriorityNone); err != nil {
		t.Fatal(err)
	}
	sm.mu.Lock()
	state := sm.states[FileKey{Hash: mt.hash, Index: 0}]
	active, ranges := state.ActiveStreams, len(state.AppliedRanges)
	sm.mu.Unlock()
	if got := e.GetTorrent(mt.hash); got.State != models.StateQueued || !got.SelectionComplete || !mt.t.DataDownloadAllowed() || active != 1 || ranges == 0 {
		t.Fatal("slot release cancelled independent stream overlay", got, active, ranges)
	}
	if mt.t.Files()[0].Priority() != torrent.PiecePriorityNone {
		t.Fatal("stream persisted as base priority")
	}
	if err := e.Pause(mt.hash); err != nil {
		t.Fatal(err)
	}
	if mt.t.DataDownloadAllowed() {
		t.Fatal("stream overlay undid Pause")
	}
}

func TestStateTransitionStaleDiskAndSlotEffectsCannotTouchReadd(t *testing.T) {
	e, old := gateFixture(t)
	if err := e.Delete(old.hash); err != nil {
		t.Fatal(err)
	}
	data := createTestTorrentBytes(4, "gate", []metainfo.FileInfo{{Length: 8, Path: []string{"video"}}}, [][]byte{[]byte("gooddata")})
	if _, err := e.AddTorrentFile(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	before := e.GetTorrent(old.hash)
	calls := 0
	e.SetDownloadBlockPersistence(func(string, string) error { calls++; return nil })
	e.admissionMu.Lock()
	err := e.blockDownloadLocked(old, "obsolete")
	faultErr := e.quiesceFaultLocked(old, stateMetadataFailed)
	e.releaseFailedSlotLocked(old)
	e.reconcileSelectionLocked(old)
	e.admissionMu.Unlock()
	after := e.GetTorrent(old.hash)
	if !errors.Is(err, ErrOperationConflict) || !errors.Is(faultErr, ErrOperationConflict) || calls != 0 || after.State != before.State || after.DownloadBlock != "" {
		t.Fatal("stale event affected new record", after, err, calls)
	}
}

func TestStateTransitionSchedulerLimitsAndFIFO(t *testing.T) {
	for _, limit := range []int{0, 1, 3} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			dir := t.TempDir()
			e, err := New(&config.Config{DownloadDir: dir, DBPath: filepath.Join(dir, "hub.db"), MaxActiveDownloads: limit, BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			var hashes []string
			for i := 0; i < 5; i++ {
				data := createTestTorrentBytes(4, fmt.Sprintf("fifo-%d", i), []metainfo.FileInfo{{Length: 4, Path: []string{"payload"}}}, [][]byte{[]byte("data")})
				m, err := e.AddTorrentFile(bytes.NewReader(data))
				if err != nil {
					t.Fatal(err)
				}
				e.SetQueuePosition(m.Hash, int64(i+1))
				hashes = append(hashes, m.Hash)
			}
			e.manageResources()
			for i, hash := range hashes {
				m := e.GetTorrent(hash)
				active := i < limit
				want := models.StateQueued
				if active {
					want = models.StateDownloading
				}
				if m.State != want || e.GetRawTorrent(hash).DataDownloadAllowed() != active {
					t.Fatalf("limit=%d FIFO index=%d phase=%s", limit, i, m.State)
				}
			}
		})
	}
}
