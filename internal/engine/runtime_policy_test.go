package engine

import (
	"errors"
	"testing"

	"torrent-stream-hub/internal/models"
)

func TestRuntimePolicyIndependentAxesAdmissionMatrix(t *testing.T) {
	for bits := 0; bits < 2048; bits++ {
		p := runtimePolicy{registered: true, run: models.RunEnabled, initializing: bits&1 != 0, checking: bits&2 != 0, uncertain: bits&4 != 0, slot: bits&8 != 0, selectedComplete: bits&16 != 0, payloadComplete: bits&32 != 0, terminal: bits&64 != 0, verificationFault: bits&128 != 0}
		if bits&256 != 0 {
			p.disk = "disk"
		}
		if bits&512 != 0 {
			p.metadataFault = models.ErrMetadataPersistence
		}
		if bits&1024 != 0 {
			p.run = models.RunPaused
		}
		for _, available := range []bool{false, true} {
			d, err := runtimeTransition(p, stateEvent{kind: stateSlotRequested, slotAvailable: available})
			want := bits == 0 && available
			if !want {
				if !errors.Is(err, ErrOperationConflict) || d != (stateDecision{}) {
					t.Fatal("rejected grant had effects", bits, available, d, err)
				}
				continue
			}
			if err != nil || !d.policy.slot || d.policy.run != p.run || d.next.state != models.StateDownloading {
				t.Fatal("grant lost independent authority", d, err)
			}
		}
	}
}

func TestRuntimePolicyFaultOwnershipAndRecovery(t *testing.T) {
	for bits := 0; bits < 32; bits++ {
		p := runtimePolicy{registered: true, run: models.RunEnabled, slot: true}
		if bits&1 != 0 {
			p.run = models.RunPaused
		}
		if bits&2 != 0 {
			p.disk = "sticky"
		}
		if bits&4 != 0 {
			p.metadataFault = models.ErrMetadataPersistence
		}
		if bits&8 != 0 {
			p.selectionFault = models.ErrInvalidSelection
		}
		if bits&16 != 0 {
			p.missingFault = models.ErrMissingFiles
		}
		begin, err := runtimeTransition(p, stateEvent{kind: stateVerification, verification: verificationBegin})
		if err != nil || begin.policy.slot || !begin.policy.checking || !begin.policy.uncertain || begin.policy.run != p.run {
			t.Fatal(begin, err)
		}
		for _, outcome := range []verificationEvent{verificationSucceeded, verificationFailed, verificationCancelled} {
			d, err := runtimeTransition(begin.policy, stateEvent{kind: stateVerification, verification: outcome})
			if err != nil || d.policy.checking || d.policy.slot || d.policy.run != p.run || d.policy.disk != p.disk || d.policy.metadataFault != p.metadataFault || d.policy.selectionFault != p.selectionFault || d.policy.missingFault != p.missingFault {
				t.Fatal("verification consumed foreign authority", bits, d, err)
			}
			if d.policy.uncertain != (outcome != verificationSucceeded) {
				t.Fatal("wrong verifier outcome", d)
			}
		}
		fault, err := runtimeTransition(p, stateEvent{kind: stateMetadataFailed})
		if err != nil || fault.policy.run != p.run || fault.policy.slot || fault.policy.missingFault != p.missingFault || fault.policy.selectionFault != p.selectionFault || fault.policy.disk != p.disk {
			t.Fatal("fault consumed other axes", fault, err)
		}
		paused, err := runtimeTransition(fault.policy, stateEvent{kind: statePaused, fullReconcile: true})
		if err != nil || paused.policy.metadataFault == "" || paused.policy.disk != p.disk || paused.next.state != models.StatePaused {
			t.Fatal(paused, err)
		}
		staged, err := runtimeTransition(paused.policy, stateEvent{kind: stateRestoreStaged})
		if err != nil || staged.policy.run != models.RunPaused || !staged.policy.initializing {
			t.Fatal("staging fabricated committed intent", staged, err)
		}
		resumed, err := runtimeTransition(staged.policy, stateEvent{kind: stateActivated, phase: models.StateQueued, fullReconcile: true, diskRecovered: true})
		if err != nil || resumed.policy.run != models.RunEnabled || resumed.policy.slot || !resumed.policy.payloadEligible() {
			t.Fatal("explicit retry invented slot or retained own fault", resumed, err)
		}
	}
}

func TestRuntimePolicyProjectionPrecedence(t *testing.T) {
	for _, tc := range []struct {
		p      runtimePolicy
		phase  models.TorrentState
		reason models.ErrorReason
	}{
		{runtimePolicy{run: models.RunPaused, disk: "disk", missingFault: models.ErrMissingFiles}, models.StatePaused, models.ErrNone},
		{runtimePolicy{run: models.RunEnabled, disk: "disk", missingFault: models.ErrMissingFiles}, models.StateDiskFull, "disk"},
		{runtimePolicy{run: models.RunEnabled, missingFault: models.ErrMissingFiles, metadataFault: models.ErrMetadataPersistence, verificationFault: true}, models.StateMissingFiles, models.ErrMissingFiles},
		{runtimePolicy{run: models.RunEnabled, metadataFault: models.ErrMetadataPersistence, selectionFault: models.ErrInvalidSelection, verificationFault: true}, models.StateError, models.ErrMetadataPersistence},
		{runtimePolicy{run: models.RunEnabled, selectedComplete: true}, models.StateQueued, models.ErrNone},
		{runtimePolicy{run: models.RunEnabled, payloadComplete: true}, models.StateSeeding, models.ErrNone},
		{runtimePolicy{run: models.RunEnabled, slot: true, checking: true, uncertain: true}, models.StateQueued, models.ErrNone},
	} {
		phase, reason := tc.p.projection()
		if phase != tc.phase || reason != tc.reason {
			t.Fatal(tc, phase, reason)
		}
	}
}

func TestRuntimePolicyIgnoresDisplayPhaseForAdmission(t *testing.T) {
	e, mt := gateFixture(t)
	if err := e.Pause(mt.hash); err != nil {
		t.Fatal(err)
	}
	e.admissionMu.Lock()
	mt.mu.Lock()
	// A deliberately corrupt transport cache cannot enable the applied run
	// authority, allocate a slot, restore base selection or reopen the gate.
	mt.state = models.StateDownloading
	mt.mu.Unlock()
	err := e.admitPayloadLocked(mt)
	grantErr := func() error {
		mt.mu.Lock()
		defer mt.mu.Unlock()
		_, err := mt.applyStateEventLocked(stateEvent{kind: stateSlotRequested, slotAvailable: true})
		return err
	}()
	e.admissionMu.Unlock()
	if !errors.Is(err, ErrDownloadBlocked) || !errors.Is(grantErr, ErrOperationConflict) || mt.t.DataDownloadAllowed() {
		t.Fatal("display cache became authority", err, grantErr)
	}
}

func TestRuntimePolicyTerminalAndUnknownEventsHaveNoEffects(t *testing.T) {
	for _, kind := range []stateEventKind{stateDeleted, stateClosed} {
		p := runtimePolicy{registered: true, run: models.RunEnabled, slot: true, checking: true, uncertain: true, disk: "sticky", metadataFault: models.ErrMetadataPersistence}
		d, err := runtimeTransition(p, stateEvent{kind: kind})
		if err != nil || !d.policy.terminal || d.policy.slot || d.policy.checking || !d.policy.uncertain || d.policy.disk != p.disk || d.policy.metadataFault != p.metadataFault || d.policy.run != p.run {
			t.Fatal(d, err)
		}
		for _, event := range []stateEvent{{kind: statePaused}, {kind: stateSlotRequested, slotAvailable: true}, {kind: stateActivated, phase: models.StateQueued, diskRecovered: true}, {kind: stateVerification, verification: verificationSucceeded}} {
			if next, err := runtimeTransition(d.policy, event); !errors.Is(err, ErrOperationConflict) || next != (stateDecision{}) {
				t.Fatal("terminal accepted event", next, err)
			}
		}
	}
	for _, event := range []stateEvent{{kind: "unknown"}, {kind: stateActivated, phase: "unknown"}, {kind: stateSelectionObserved}, {kind: stateSelectionObserved, metadataKnown: true, payloadComplete: true}} {
		if d, err := runtimeTransition(runtimePolicy{registered: true, run: models.RunEnabled}, event); !errors.Is(err, ErrOperationConflict) || d != (stateDecision{}) {
			t.Fatal("unknown/invalid event accepted", d, err)
		}
	}
}

// Keep the frozen phase model connected to the new policy only as a regression
// oracle for supported historical sequences. It has no production entry point;
// independent-axis matrices above also cover combinations the old phase model
// could not represent (multiple fault owners and a separate slot authority).
func TestRuntimePolicyCompatibleSequenceAgainstFrozenReference(t *testing.T) {
	var p runtimePolicy
	var old stateFacts
	for _, event := range []stateEvent{
		{kind: stateRegistered, phase: models.StateQueued},
		{kind: stateSlotRequested, slotAvailable: true},
		{kind: stateSelectionObserved, metadataKnown: true, selectedComplete: true},
		{kind: statePaused, fullReconcile: true},
		{kind: stateActivated, phase: models.StateQueued, fullReconcile: true},
		{kind: stateVerification, verification: verificationBegin},
		{kind: stateVerification, verification: verificationSucceeded},
		{kind: stateDiskBlocked, diskReason: "sticky"},
		{kind: statePaused, fullReconcile: true},
		{kind: stateActivated, phase: models.StateQueued, fullReconcile: true, diskRecovered: true},
		{kind: stateClosed},
	} {
		d, err := runtimeTransition(p, event)
		reference, referenceErr := stateTransition(old, event)
		if err != nil || referenceErr != nil {
			t.Fatal(event, err, referenceErr)
		}
		if d.verificationDecision != reference.verificationDecision || d.selectionComplete != reference.selectionComplete || d.resetDownload != reference.resetDownload || d.clearBasePriorities != reference.clearBasePriorities || d.invalidateApplied != reference.invalidateApplied || d.terminal != reference.terminal {
			t.Fatalf("compatibility drift event=%+v new=%+v old=%+v", event, d, reference)
		}
		p = d.policy
		old = stateFacts{verificationFacts: reference.next, selectionComplete: reference.selectionComplete, terminal: reference.terminal}
	}
}
