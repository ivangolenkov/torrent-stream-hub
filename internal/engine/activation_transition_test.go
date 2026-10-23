package engine

import (
	"errors"
	"testing"

	"torrent-stream-hub/internal/models"
)

func TestActivationTransitionPolicy(t *testing.T) {
	for _, phase := range []models.TorrentState{models.StateQueued, models.StateDownloading, models.StateSeeding, models.StateStreaming, models.StatePaused, models.StateError, models.StateMissingFiles, models.StateDiskFull} {
		for flags := 0; flags < 32; flags++ {
			f := stateFacts{verificationFacts: verificationFacts{state: models.StatePaused, err: models.ErrNone, checking: flags&1 != 0, uncertain: flags&2 != 0, initializing: flags&4 != 0}}
			if flags&8 != 0 {
				f.diskBlock = "sticky"
			}
			event := stateEvent{kind: stateActivated, phase: phase, err: models.ErrActivationBlocked, fullReconcile: flags&16 != 0}
			d, err := stateTransition(f, event)
			if f.checking || f.uncertain || f.initializing && !event.fullReconcile {
				if !errors.Is(err, ErrOperationConflict) || d != (stateDecision{}) {
					t.Fatal("activation bypassed veto", f, event, d, err)
				}
				continue
			}
			want := phase
			if want == models.StateDownloading || want == models.StateSeeding || want == models.StateStreaming {
				want = models.StateQueued
			}
			if want != models.StatePaused && f.diskBlock != "" {
				want = models.StateDiskFull
			}
			if err != nil || d.next.state != want || d.next.initializing || d.next.diskBlock != f.diskBlock || !d.closePayload || !d.resetDownload {
				t.Fatal("bad activation", f, event, d, err)
			}
			if want == models.StateQueued || want == models.StatePaused {
				if d.next.err != models.ErrNone {
					t.Fatal("stale error retained", d)
				}
			}
			if want == models.StateDownloading || want == models.StateSeeding {
				t.Fatal("restore invented readiness/slot")
			}
		}
	}
	for _, recovered := range []bool{false, true} {
		d, err := stateTransition(stateFacts{verificationFacts: verificationFacts{diskBlock: "sticky"}}, stateEvent{kind: stateActivated, phase: models.StateQueued, diskRecovered: recovered})
		if err != nil || (d.next.diskBlock == "") != recovered {
			t.Fatal("disk recovery authority lost", d, err)
		}
	}
	if d, err := stateTransition(stateFacts{}, stateEvent{kind: stateActivated, phase: "unknown"}); !errors.Is(err, ErrOperationConflict) || d != (stateDecision{}) {
		t.Fatal("unknown phase accepted", d, err)
	}
}

func TestRegistrationAndTerminalPolicy(t *testing.T) {
	for _, staged := range []bool{false, true} {
		d, err := stateTransition(stateFacts{}, stateEvent{kind: stateRegistered, phase: models.StateDownloading, staged: staged})
		if err != nil || d.next.state != models.StateQueued || d.next.initializing != staged || !d.closePayload || d.closeUpload != staged {
			t.Fatal(d, err)
		}
	}
	if d, err := stateTransition(stateFacts{verificationFacts: verificationFacts{state: models.StateQueued}}, stateEvent{kind: stateRegistered, phase: models.StateQueued}); !errors.Is(err, ErrOperationConflict) || d != (stateDecision{}) {
		t.Fatal("double registration accepted", d, err)
	}
	for _, kind := range []stateEventKind{stateDeleted, stateClosed} {
		f := stateFacts{verificationFacts: verificationFacts{state: models.StateDownloading, checking: true, uncertain: true, diskBlock: "sticky"}}
		d, err := stateTransition(f, stateEvent{kind: kind})
		if err != nil || !d.terminal || !d.invalidateApplied || !d.closePayload || !d.closeUpload || d.next.state != models.StateQueued || d.next.diskBlock != "sticky" || !d.next.uncertain {
			t.Fatal("terminal lost safety", d, err)
		}
		for _, next := range []stateEventKind{stateRegistered, stateActivated, statePaused, stateRestoreStaged, stateVerification, stateSelectionObserved, stateSlotRequested, stateDiskBlocked, stateMetadataFailed, statePayloadMissing, stateDeleted, stateClosed} {
			if got, err := stateTransition(stateFacts{verificationFacts: d.next, terminal: true}, stateEvent{kind: next, phase: models.StateQueued}); !errors.Is(err, ErrOperationConflict) || got != (stateDecision{}) {
				t.Fatal("terminal accepted event", next, got, err)
			}
		}
	}
}

func TestFinishRestoreRejectsUnknownPhaseBeforeEffects(t *testing.T) {
	e, mt := gateFixture(t)
	before := e.GetTorrent(mt.hash)
	allowed := mt.t.DataDownloadAllowed()
	calls := 0
	e.SetDownloadBlockPersistence(func(string, string) error { calls++; return nil })
	if err := e.FinishRestore(mt.hash, "unknown", true); !errors.Is(err, ErrOperationConflict) {
		t.Fatal(err)
	}
	after := e.GetTorrent(mt.hash)
	if calls != 0 || before.State != after.State || mt.t.DataDownloadAllowed() != allowed {
		t.Fatal("rejected restore performed effects", after, calls)
	}
}

func TestDeleteAndCloseTerminalizeOldRuntime(t *testing.T) {
	for _, closeEngine := range []bool{false, true} {
		e, mt := gateFixture(t)
		if closeEngine {
			e.Close()
		} else if err := e.Delete(mt.hash); err != nil {
			t.Fatal(err)
		}
		mt.mu.Lock()
		_, err := mt.applyStateEventLocked(stateEvent{kind: stateActivated, phase: models.StateQueued, fullReconcile: true, diskRecovered: true})
		terminal, applied := mt.terminal, mt.appliedIntentRevision
		mt.mu.Unlock()
		if !terminal || applied != 0 || !errors.Is(err, ErrOperationConflict) || mt.t.DataDownloadAllowed() {
			t.Fatal("terminal runtime reopened", terminal, applied, err)
		}
	}
}
