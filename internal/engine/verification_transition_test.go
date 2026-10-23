package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/anacrolix/torrent"
	"torrent-stream-hub/internal/models"
)

func TestVerificationTransitionPolicy(t *testing.T) {
	for _, tc := range []struct {
		name                                  string
		facts                                 verificationFacts
		event                                 verificationEvent
		state                                 models.TorrentState
		err                                   models.ErrorReason
		checking, uncertain, upload, conflict bool
	}{
		{name: "begin releases slot", facts: verificationFacts{state: models.StateDownloading}, event: verificationBegin, state: models.StateQueued, checking: true, uncertain: true},
		{name: "begin revokes seeding", facts: verificationFacts{state: models.StateSeeding}, event: verificationBegin, state: models.StateQueued, checking: true, uncertain: true},
		{name: "concurrent begin", facts: verificationFacts{checking: true}, event: verificationBegin, conflict: true},
		{name: "initializing begin", facts: verificationFacts{initializing: true}, event: verificationBegin, conflict: true},
		{name: "unsolicited completion", event: verificationSucceeded, conflict: true},
		{name: "unknown", event: 255, conflict: true},
		{name: "failure is not invalid torrent", facts: verificationFacts{state: models.StateQueued, checking: true}, event: verificationFailed, state: models.StateError, err: models.ErrVerificationFailed, uncertain: true},
		{name: "cancellation veto", facts: verificationFacts{state: models.StateQueued, checking: true}, event: verificationCancelled, state: models.StateError, err: models.ErrVerificationFailed, uncertain: true},
		{name: "success clears own error", facts: verificationFacts{state: models.StateError, err: models.ErrVerificationFailed, checking: true, uncertain: true}, event: verificationSucceeded, state: models.StateQueued, upload: true},
		{name: "success preserves foreign error", facts: verificationFacts{state: models.StateError, err: models.ErrInvalidTorrent, checking: true}, event: verificationSucceeded, state: models.StateError, err: models.ErrInvalidTorrent},
		{name: "failure preserves foreign error", facts: verificationFacts{state: models.StateError, err: models.ErrInvalidTorrent, checking: true}, event: verificationFailed, state: models.StateError, err: models.ErrInvalidTorrent, uncertain: true},
		{name: "success preserves missing files", facts: verificationFacts{state: models.StateMissingFiles, err: models.ErrMissingFiles, checking: true}, event: verificationSucceeded, state: models.StateMissingFiles, err: models.ErrMissingFiles},
		{name: "pause and disk survive success", facts: verificationFacts{state: models.StatePaused, diskBlock: "sticky", checking: true}, event: verificationSucceeded, state: models.StatePaused},
		{name: "pause survives failure", facts: verificationFacts{state: models.StatePaused, checking: true}, event: verificationFailed, state: models.StatePaused, uncertain: true},
		{name: "disk dominates failed verification", facts: verificationFacts{state: models.StateQueued, diskBlock: "sticky", checking: true}, event: verificationFailed, state: models.StateDiskFull, err: "sticky", uncertain: true},
		{name: "disk upload after success", facts: verificationFacts{state: models.StateDiskFull, diskBlock: "sticky", checking: true}, event: verificationSucceeded, state: models.StateDiskFull, err: "sticky", upload: true},
		{name: "apply failure vetoes upload", facts: verificationFacts{state: models.StateQueued, initializing: true, checking: true}, event: verificationSucceeded, state: models.StateQueued},
		{name: "restored block releases slot", facts: verificationFacts{state: models.StateDownloading}, event: verificationRestored, state: models.StateQueued, uncertain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := verificationTransition(tc.facts, tc.event)
			if tc.conflict {
				if !errors.Is(err, ErrOperationConflict) {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			n := d.next
			if n.state != tc.state || n.err != tc.err || n.checking != tc.checking || n.uncertain != tc.uncertain || d.allowUpload != tc.upload || d.closeUpload == tc.upload || !d.closePayload {
				t.Fatalf("unexpected decision: %+v", d)
			}
			if n.diskBlock != tc.facts.diskBlock || n.initializing != tc.facts.initializing {
				t.Fatal("mutated foreign axis", d)
			}
		})
	}
}

// Keep actual task callbacks behind a barrier; invoke the executor synchronously
// with their captured capability to test each interleaving without polling.
func controlledVerification(t *testing.T, e *Engine, mt *ManagedTorrent) *verificationAttempt {
	t.Helper()
	release := make(chan struct{})
	e.admissionMu.Lock()
	e.verifyTorrent = func(ctx context.Context, _ *torrent.Torrent) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	e.admissionMu.Unlock()
	t.Cleanup(func() { close(release) })
	if _, err := e.StartRecheck(mt.hash); err != nil {
		t.Fatal(err)
	}
	mt.mu.Lock()
	attempt := mt.verificationAttempt
	mt.mu.Unlock()
	if attempt == nil {
		t.Fatal("no attempt capability")
	}
	return attempt
}

func TestVerificationRetryRecoversOnlyOwnErrorAndRejectsDuplicateCompletion(t *testing.T) {
	e, mt := gateFixture(t)
	persistCalls := 0
	e.SetVerificationPersistence(func(_ models.RuntimeFence, _ bool, _ string) error { persistCalls++; return nil })
	first := controlledVerification(t, e, mt)
	if got := e.GetTorrent(mt.hash); got.State != models.StateQueued || mt.t.DataDownloadAllowed() {
		t.Fatal("begin retained slot/admission", got)
	}
	if err := e.finishVerification(mt, first, errors.New("injected verifier failure")); err == nil {
		t.Fatal("lost verifier failure")
	}
	got := e.GetTorrent(mt.hash)
	if got.State != models.StateError || got.Error != models.ErrVerificationFailed {
		t.Fatal("misclassified verification failure", got)
	}
	second := controlledVerification(t, e, mt)
	before := persistCalls
	if err := e.finishVerification(mt, first, nil); !errors.Is(err, ErrOperationConflict) {
		t.Fatal("duplicate completion accepted", err)
	}
	if persistCalls != before {
		t.Fatal("stale completion changed durable verification key")
	}
	mt.mu.Lock()
	active, uncertain := mt.verificationAttempt == second, mt.verificationUncertain
	mt.mu.Unlock()
	if !active || !uncertain || mt.t.DataDownloadAllowed() {
		t.Fatal("old callback finalized later attempt")
	}
	if err := e.finishVerification(mt, second, nil); err != nil {
		t.Fatal(err)
	}
	got = e.GetTorrent(mt.hash)
	if got.State == models.StateError || got.Error != models.ErrNone {
		t.Fatal("successful local retry stranded Error", got)
	}
}

func TestVerificationCompletionPersistenceFailureKeepsVetoAndReleasesSlot(t *testing.T) {
	e, mt := gateFixture(t)
	e.SetVerificationPersistence(func(_ models.RuntimeFence, blocked bool, _ string) error {
		if !blocked {
			return errors.New("injected persistence failure")
		}
		return nil
	})
	attempt := controlledVerification(t, e, mt)
	if err := e.finishVerification(mt, attempt, nil); err == nil {
		t.Fatal("ignored persistence failure")
	}
	e.manageResources()
	mt.mu.Lock()
	uncertain, reserved := mt.verificationUncertain, mt.state == models.StateDownloading
	mt.mu.Unlock()
	if !uncertain || reserved || mt.t.DataDownloadAllowed() {
		t.Fatal("failed durable removal admitted payload")
	}
}

func TestRestoredVerificationVetoCannotReserveSchedulerSlot(t *testing.T) {
	e, mt := gateFixture(t)
	if err := e.RestoreVerificationBlock(mt.hash); err != nil {
		t.Fatal(err)
	}
	e.manageResources()
	if got := e.GetTorrent(mt.hash); got.State != models.StateQueued || mt.t.DataDownloadAllowed() {
		t.Fatal("scheduler granted uncertain runtime", got)
	}
}

func TestVerificationCompletionUsesCurrentPauseAndDiskFacts(t *testing.T) {
	e, mt := gateFixture(t)
	attempt := controlledVerification(t, e, mt)
	if err := e.Pause(mt.hash); err != nil {
		t.Fatal(err)
	}
	e.admissionMu.Lock()
	err := e.blockDownloadLocked(mt, "sticky disk latch")
	e.admissionMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.finishVerification(mt, attempt, nil); err != nil {
		t.Fatal(err)
	}
	got := e.GetTorrent(mt.hash)
	if got.State != models.StatePaused || got.DownloadBlock != "sticky disk latch" || mt.t.DataDownloadAllowed() {
		t.Fatal("verification restored entry state or cleared foreign blocker", got)
	}
}

func TestVerificationBeginPersistenceFailureNeverRunsVerifier(t *testing.T) {
	e, mt := gateFixture(t)
	e.SetVerificationPersistence(func(models.RuntimeFence, bool, string) error { return errors.New("injected begin persistence failure") })
	e.admissionMu.Lock()
	e.verifyTorrent = func(context.Context, *torrent.Torrent) error {
		t.Error("verifier ran without durable block")
		return nil
	}
	e.admissionMu.Unlock()
	if _, err := e.StartRecheck(mt.hash); err == nil {
		t.Fatal("reported success without durable block")
	}
	e.manageResources()
	mt.mu.Lock()
	checking, uncertain, attempt, state := mt.rechecking, mt.verificationUncertain, mt.verificationAttempt, mt.state
	mt.mu.Unlock()
	if checking || !uncertain || attempt != nil || state == models.StateDownloading || mt.t.DataDownloadAllowed() {
		t.Fatal("failed begin retained task/slot or opened gate")
	}
}
