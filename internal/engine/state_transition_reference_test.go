package engine

import (
	"torrent-stream-hub/internal/models"
)

// Frozen phase-based reference model. It is compiled ONLY into tests, to keep
// the historical transition matrices as regression oracles during axis migration.
type stateFacts struct {
	verificationFacts
	selectionComplete bool
	terminal          bool
}

// I/O-free common policy entry point. Verification's rules remain a sub-policy,
// not a second mutation path; all integrated producers share the executor below.
func stateTransition(f stateFacts, event stateEvent) (stateDecision, error) {
	reject := func(reason string) (stateDecision, error) {
		return stateDecision{}, StateTransitionError{Event: string(event.kind), Reason: reason}
	}
	d := stateDecision{verificationDecision: verificationDecision{next: f.verificationFacts}, selectionComplete: f.selectionComplete}
	if f.terminal {
		return reject("runtime_terminal")
	}
	switch event.kind {
	case stateRegistered, stateActivated:
		phase := event.phase
		switch phase {
		case models.StateDownloading, models.StateSeeding, models.StateStreaming:
			phase = models.StateQueued
		case models.StateQueued, models.StatePaused, models.StateError, models.StateMissingFiles, models.StateDiskFull:
		default:
			return reject("unknown_activation_phase")
		}
		if event.kind == stateRegistered {
			if f.state != "" {
				return reject("already_registered")
			}
			d.next.diskBlock = event.diskReason
			d.next.initializing = event.staged
		} else {
			if f.checking || f.uncertain {
				return reject("verification_veto")
			}
			if f.initializing && !event.fullReconcile {
				return reject("initialization_veto")
			}
			d.next.initializing = false
			if event.diskRecovered {
				d.next.diskBlock = ""
			}
		}
		d.next.state, d.next.err = phase, event.err
		if phase == models.StateQueued || phase == models.StatePaused {
			d.next.err = models.ErrNone
		}
		if phase != models.StatePaused && d.next.diskBlock != "" {
			d.next.state, d.next.err = models.StateDiskFull, models.ErrorReason(d.next.diskBlock)
		}
		d.resetDownload, d.closePayload = true, true
		d.closeUpload = d.next.initializing || d.next.state == models.StatePaused || d.next.state == models.StateError || d.next.state == models.StateMissingFiles
		d.allowUpload = !d.closeUpload
	case stateDeleted, stateClosed:
		d.terminal = true
		d.next.initializing = true
		if f.checking {
			d.next.checking, d.next.uncertain = false, true
		}
		if f.state == models.StateDownloading || f.state == models.StateSeeding {
			d.next.state, d.next.err = models.StateQueued, models.ErrNone
		}
		d.resetDownload, d.closePayload, d.closeUpload, d.clearBasePriorities, d.invalidateApplied = true, true, true, true, true
	case stateVerification:
		v, err := verificationTransition(f.verificationFacts, event.verification)
		if err != nil {
			return reject("verification_conflict")
		}
		d.verificationDecision = v
		d.resetDownload = true
		if event.verification == verificationBegin {
			d.selectionComplete = false
		}
	case stateSelectionObserved:
		if !event.metadataKnown {
			return reject("metadata_unknown")
		}
		if event.payloadComplete && !event.selectedComplete {
			return reject("inconsistent_completion")
		}
		if f.initializing || f.checking || f.uncertain {
			return reject("runtime_veto")
		}
		d.selectionComplete = event.selectedComplete
		eligible := f.state == models.StateQueued || f.state == models.StateDownloading || f.state == models.StateSeeding
		if eligible && f.diskBlock == "" {
			if event.payloadComplete {
				d.next.state, d.next.err = models.StateSeeding, models.ErrNone
			} else if event.selectedComplete || f.state == models.StateSeeding {
				d.next.state, d.next.err = models.StateQueued, models.ErrNone
				d.resetDownload = true
				d.clearBasePriorities = f.state != models.StateQueued
				d.closeBackgroundPayload = d.clearBasePriorities
			}
		}
	case stateSlotRequested:
		if f.state != models.StateQueued {
			return reject("phase_not_queued")
		}
		if f.initializing || f.checking || f.uncertain || f.diskBlock != "" {
			return reject("runtime_veto")
		}
		if f.selectionComplete {
			return reject("selection_complete")
		}
		if !event.slotAvailable {
			return reject("capacity_unavailable")
		}
		d.next.state, d.next.err = models.StateDownloading, models.ErrNone
	case stateSlotReleased:
		if f.state != models.StateDownloading {
			return reject("slot_not_owned")
		}
		d.next.state, d.next.err = models.StateQueued, models.ErrNone
		d.resetDownload, d.clearBasePriorities, d.closeBackgroundPayload = true, true, true
	case stateDiskBlocked:
		if event.diskReason == "" {
			return reject("empty_disk_reason")
		}
		d.next.diskBlock = event.diskReason
		if f.state != models.StatePaused {
			d.next.state, d.next.err = models.StateDiskFull, models.ErrorReason(event.diskReason)
		}
		d.resetDownload, d.closePayload = true, true
	case statePaused:
		d.next.state, d.next.err = models.StatePaused, models.ErrNone
		if event.fullReconcile {
			d.next.initializing = false
		}
		d.resetDownload, d.closePayload, d.closeUpload, d.clearBasePriorities, d.invalidateApplied = true, true, true, true, true
	case stateRestoreStaged:
		if f.checking || f.uncertain {
			return reject("verification_veto")
		}
		d.next.initializing = true
		if f.state == models.StateDownloading || f.state == models.StateSeeding {
			d.next.state, d.next.err = models.StateQueued, models.ErrNone
		}
		d.resetDownload, d.closePayload, d.closeUpload, d.clearBasePriorities, d.invalidateApplied = true, true, true, true, true
	case stateApplyFailed, stateMetadataFailed, stateSelectionInvalid, statePayloadMissing:
		d.next.initializing = true
		if event.kind == stateApplyFailed {
			if f.state == models.StateDownloading || f.state == models.StateSeeding || f.state == models.StateStreaming {
				d.next.state, d.next.err = models.StateQueued, models.ErrNone
			}
		} else if f.state != models.StatePaused {
			switch {
			case f.diskBlock != "":
				d.next.state, d.next.err = models.StateDiskFull, models.ErrorReason(f.diskBlock)
			case f.state == models.StateMissingFiles:
			case f.state == models.StateError:
			default:
				d.next.state = models.StateError
				if event.kind == stateMetadataFailed {
					d.next.err = models.ErrMetadataPersistence
				} else if event.kind == statePayloadMissing {
					d.next.state, d.next.err = models.StateMissingFiles, models.ErrMissingFiles
				} else {
					d.next.err = models.ErrInvalidSelection
				}
			}
		}
		d.resetDownload, d.closePayload, d.closeUpload, d.clearBasePriorities, d.invalidateApplied = true, true, true, true, true
	default:
		return reject("unknown_event")
	}
	return d, nil
}
