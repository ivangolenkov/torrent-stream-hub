package engine

import (
	"fmt"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"
)

type stateEventKind string

const (
	stateVerification      stateEventKind = "verification"
	stateSelectionObserved stateEventKind = "selection_observed"
	stateSlotRequested     stateEventKind = "slot_requested"
	stateSlotReleased      stateEventKind = "slot_released"
	stateDiskBlocked       stateEventKind = "disk_blocked"
	statePaused            stateEventKind = "paused"
	stateRestoreStaged     stateEventKind = "restore_staged"
	stateApplyFailed       stateEventKind = "apply_failed"
	stateMetadataFailed    stateEventKind = "metadata_failed"
	stateSelectionInvalid  stateEventKind = "selection_invalid"
	stateRegistered        stateEventKind = "registered"
	stateActivated         stateEventKind = "activated"
	stateDeleted           stateEventKind = "deleted"
	stateClosed            stateEventKind = "closed"
	statePayloadMissing    stateEventKind = "payload_missing"
)

type stateEvent struct {
	selectionEmpty                                   bool
	kind                                             stateEventKind
	verification                                     verificationEvent
	metadataKnown, selectedComplete, payloadComplete bool
	slotAvailable                                    bool
	diskReason                                       string
	fullReconcile                                    bool
	phase                                            models.TorrentState
	err                                              models.ErrorReason
	staged, diskRecovered                            bool
}

type stateDecision struct {
	verificationDecision
	policy                                                     runtimePolicy
	selectionComplete                                          bool
	resetDownload, clearBasePriorities, closeBackgroundPayload bool
	invalidateApplied, terminal                                bool
}

type StateTransitionError struct{ Event, Reason string }

func (e StateTransitionError) Error() string {
	return fmt.Sprintf("state event %s rejected: %s", e.Event, e.Reason)
}
func (e StateTransitionError) Unwrap() error { return ErrOperationConflict }

// admissionMu and mt.mu are held. The policy owns independent facts; the fields
// copied below are compatibility projections for transport/diagnostics only.
func (mt *ManagedTorrent) applyStateEventLocked(event stateEvent) (stateDecision, error) {
	d, err := runtimeTransition(mt.policy, event)
	if err != nil {
		return d, err
	}
	if mt.policy != d.policy {
		mt.policyEpoch++
	}
	mt.policy = d.policy
	if mt.policy.slot && event.kind == stateSlotRequested {
		mt.slotToken++
	}
	mt.rechecking, mt.verificationUncertain = d.next.checking, d.next.uncertain
	mt.initializing, mt.downloadBlock = d.next.initializing, d.next.diskBlock
	mt.selectionComplete = d.selectionComplete
	oldState, oldErr := mt.state, mt.err
	mt.state, mt.err = d.next.state, d.next.err
	mt.terminal = d.terminal
	if event.kind == stateActivated && event.diskRecovered {
		mt.safetyBlocks = nil
		mt.expectedPayload = nil
	}
	if oldState != mt.state || oldErr != mt.err {
		logging.Infof("torrent state change hash=%s state=%s->%s error=%s event=%s", mt.hash, oldState, mt.state, mt.err, event.kind)
	}
	if d.resetDownload {
		mt.downloadAllStarted = false
	}
	if d.invalidateApplied {
		mt.appliedIntentRevision = 0
	}
	return d, nil
}
