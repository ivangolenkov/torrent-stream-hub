package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/anacrolix/torrent"
	"torrent-stream-hub/internal/models"
)

func TestQuiesceTransitionPolicy(t *testing.T) {
	for _, phase := range []models.TorrentState{models.StateQueued, models.StateDownloading, models.StateSeeding, models.StatePaused, models.StateError, models.StateMissingFiles, models.StateDiskFull} {
		for flags := 0; flags < 16; flags++ {
			f := stateFacts{verificationFacts: verificationFacts{state: phase, err: "foreign diagnostic", checking: flags&1 != 0, uncertain: flags&2 != 0, initializing: flags&4 != 0}, selectionComplete: true}
			if flags&8 != 0 {
				f.diskBlock = "sticky"
			}
			for _, kind := range []stateEventKind{statePaused, stateRestoreStaged, stateApplyFailed, stateMetadataFailed, stateSelectionInvalid, statePayloadMissing} {
				d, err := stateTransition(f, stateEvent{kind: kind})
				if kind == stateRestoreStaged && flags&3 != 0 {
					if !errors.Is(err, ErrOperationConflict) || d != (stateDecision{}) {
						t.Fatal("stage ignored verifier veto", d, err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if !d.closePayload || !d.closeUpload || !d.clearBasePriorities || !d.resetDownload || !d.invalidateApplied {
					t.Fatal("quiesce missing effects", kind, d)
				}
				if d.next.diskBlock != f.diskBlock || d.next.checking != f.checking || d.next.uncertain != f.uncertain || !d.selectionComplete {
					t.Fatal("quiesce cleared foreign safety axis", kind, d)
				}
				if kind == statePaused {
					if d.next.state != models.StatePaused || d.next.err != models.ErrNone || d.next.initializing != f.initializing {
						t.Fatal(d)
					}
					full, err := stateTransition(f, stateEvent{kind: statePaused, fullReconcile: true})
					if err != nil || full.next.initializing || full.next.uncertain != f.uncertain || full.next.diskBlock != f.diskBlock {
						t.Fatal("full Pause exceeded authority", full, err)
					}
				} else if !d.next.initializing {
					t.Fatal("fault/stage lost initialization veto", d)
				}
				if kind == stateApplyFailed || kind == stateRestoreStaged {
					want := phase
					if phase == models.StateDownloading || phase == models.StateSeeding {
						want = models.StateQueued
					}
					if d.next.state != want {
						t.Fatal("wrong slot release phase", kind, d)
					}
				}
				if kind == stateMetadataFailed || kind == stateSelectionInvalid || kind == statePayloadMissing {
					want, reason := models.StateError, models.ErrMetadataPersistence
					if kind == stateSelectionInvalid {
						reason = models.ErrInvalidSelection
					}
					if kind == statePayloadMissing {
						want, reason = models.StateMissingFiles, models.ErrMissingFiles
					}
					if phase == models.StatePaused {
						want, reason = phase, f.err
					} else if f.diskBlock != "" {
						want, reason = models.StateDiskFull, models.ErrorReason(f.diskBlock)
					} else if phase == models.StateError || phase == models.StateMissingFiles {
						want, reason = phase, f.err
					}
					if d.next.state != want || d.next.err != reason {
						t.Fatal("fault overwrote foreign display cause", kind, d)
					}
				}
			}
		}
	}
}

func TestStagedRestoreQuiescesAndRequiresFullApplyBeforeAcknowledgment(t *testing.T) {
	e, mt := gateFixture(t)
	f := models.RuntimeFence{IntentVersion: models.IntentVersion{Hash: mt.hash, RecordID: "record", Revision: 1}, Attachment: "live"}
	if err := e.BindRuntime(mt.t, f); err != nil {
		t.Fatal(err)
	}
	saved := e.GetTorrent(mt.hash)
	mt.mu.Lock()
	oldPayload := mt.payloadCtx
	mt.mu.Unlock()
	if err := e.StageExistingRestore(saved); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(context.Cause(oldPayload), ErrDownloadBlocked) {
		t.Fatal("staging did not cancel old payload work", context.Cause(oldPayload))
	}
	got := e.GetTorrent(mt.hash)
	if got.State != models.StateQueued || got.Control.AppliedRevision != 0 || mt.t.DataDownloadAllowed() || mt.t.Files()[0].Priority() != torrent.PiecePriorityNone {
		t.Fatal("staging retained admission/reservation/acknowledgment", got)
	}
	if err := e.BindRuntime(mt.t, f); !errors.Is(err, ErrOperationConflict) {
		t.Fatal("staged runtime acknowledged", err)
	}
	if err := e.Pause(mt.hash); err != nil {
		t.Fatal(err)
	}
	mt.mu.Lock()
	initializing := mt.initializing
	mt.mu.Unlock()
	if !initializing {
		t.Fatal("delta Pause removed incomplete-apply veto")
	}
	if err := e.ReconcilePausedIntent(saved); err != nil {
		t.Fatal(err)
	}
	if err := e.BindRuntime(mt.t, f); err != nil {
		t.Fatal("full Pause did not permit acknowledgment", err)
	}
	if got := e.GetTorrent(mt.hash); got.State != models.StatePaused || got.Control.AppliedRevision != 1 || mt.t.DataDownloadAllowed() {
		t.Fatal(got)
	}
}

func TestStagingVerificationConflictHasNoPriorityOrGateEffects(t *testing.T) {
	e, mt := gateFixture(t)
	attempt := controlledVerification(t, e, mt)
	saved := e.GetTorrent(mt.hash)
	saved.Files[0].Priority = models.PriorityNone
	before := e.GetTorrent(mt.hash)
	if err := e.StageExistingRestore(saved); !errors.Is(err, ErrOperationConflict) {
		t.Fatal(err)
	}
	after := e.GetTorrent(mt.hash)
	if after.State != before.State || after.Files[0].Priority != before.Files[0].Priority || mt.t.DataDownloadAllowed() {
		t.Fatal("rejected stage partially applied selection", after)
	}
	if err := e.finishVerification(mt, attempt, nil); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidSelectionFaultClosesAdmissionAndFullPauseRepairsMap(t *testing.T) {
	e, mt := gateFixture(t)
	saved := e.GetTorrent(mt.hash)
	e.admissionMu.Lock()
	mt.mu.Lock()
	mt.filePriorities[999] = models.PriorityHigh
	mt.mu.Unlock()
	err := e.applyFilePrioritiesAndDownload(mt)
	e.admissionMu.Unlock()
	if err == nil {
		t.Fatal("invalid map accepted")
	}
	got := e.GetTorrent(mt.hash)
	if got.State != models.StateError || got.Error != models.ErrInvalidSelection || got.Control.AppliedRevision != 0 || mt.t.DataDownloadAllowed() || mt.t.Files()[0].Priority() != torrent.PiecePriorityNone {
		t.Fatal("invalid selection fault not quiescent", got)
	}
	if err := e.ReconcilePausedIntent(saved); err != nil {
		t.Fatal(err)
	}
	mt.mu.Lock()
	_, invalid := mt.filePriorities[999]
	initializing := mt.initializing
	mt.mu.Unlock()
	if invalid || initializing || e.GetTorrent(mt.hash).State != models.StatePaused {
		t.Fatal("full Pause did not repair selection")
	}
}

func TestMetadataFaultPreservesPauseDiskAndVerificationVeto(t *testing.T) {
	e, mt := gateFixture(t)
	if err := e.Pause(mt.hash); err != nil {
		t.Fatal(err)
	}
	if err := e.RestoreVerificationBlock(mt.hash); err != nil {
		t.Fatal(err)
	}
	e.admissionMu.Lock()
	err := e.blockDownloadLocked(mt, "sticky")
	if err == nil {
		err = e.quiesceFaultLocked(mt, stateMetadataFailed)
	}
	e.admissionMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	got := e.GetTorrent(mt.hash)
	mt.mu.Lock()
	uncertain, initializing := mt.verificationUncertain, mt.initializing
	mt.mu.Unlock()
	if got.State != models.StatePaused || got.DownloadBlock != "sticky" || !uncertain || !initializing || mt.t.DataDownloadAllowed() {
		t.Fatal("fault cleared independent blockers", got)
	}
}
