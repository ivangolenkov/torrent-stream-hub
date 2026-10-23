package engine

import "torrent-stream-hub/internal/models"

// Frozen compatibility reference, compiled only into tests. Production uses the
// independent-axis runtimeTransition, not a phase-based verification sub-policy.
func verificationTransition(f verificationFacts, event verificationEvent) (verificationDecision, error) {
	n := f
	switch event {
	case verificationBegin:
		if f.checking || f.initializing {
			return verificationDecision{}, ErrOperationConflict
		}
		n.checking, n.uncertain = true, true
	case verificationSucceeded, verificationFailed, verificationCancelled:
		if !f.checking {
			return verificationDecision{}, ErrOperationConflict
		}
		n.checking = false
		n.uncertain = event != verificationSucceeded
	case verificationRestored:
		n.uncertain = true
	default:
		return verificationDecision{}, ErrOperationConflict
	}
	// A verifier never owns a scheduler slot, including while persistence or task
	// admission fails. Subsequent admission is a fresh scheduler decision.
	if n.state == models.StateDownloading || n.state == models.StateSeeding {
		n.state, n.err = models.StateQueued, models.ErrNone
	}
	if n.state != models.StatePaused {
		switch {
		case n.diskBlock != "":
			n.state, n.err = models.StateDiskFull, models.ErrorReason(n.diskBlock)
		case n.state == models.StateError && n.err != models.ErrVerificationFailed:
			// An unrelated safety error is outside this producer's authority.
		case n.state == models.StateMissingFiles:
		case event == verificationFailed || event == verificationCancelled:
			n.state, n.err = models.StateError, models.ErrVerificationFailed
		case event == verificationSucceeded && n.err == models.ErrVerificationFailed:
			n.state, n.err = models.StateQueued, models.ErrNone
		}
	}
	allowUpload := !n.initializing && !n.checking && !n.uncertain && n.state != models.StatePaused && n.state != models.StateError && n.state != models.StateMissingFiles
	return verificationDecision{next: n, closePayload: true, closeUpload: !allowUpload, allowUpload: allowUpload}, nil
}
