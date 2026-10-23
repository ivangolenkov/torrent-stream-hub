package engine

import "torrent-stream-hub/internal/models"

type verificationEvent uint8

const (
	verificationBegin verificationEvent = iota + 1
	verificationSucceeded
	verificationFailed
	verificationCancelled
	verificationRestored
)

// Effect/transport projection of the independent-axis policy, not its input.
type verificationFacts struct {
	state                             models.TorrentState
	err                               models.ErrorReason
	checking, uncertain, initializing bool
	diskBlock                         string
}

type verificationDecision struct {
	next                                   verificationFacts
	closePayload, closeUpload, allowUpload bool
}

func (mt *ManagedTorrent) applyVerificationEventLocked(event verificationEvent) (verificationDecision, error) {
	d, err := mt.applyStateEventLocked(stateEvent{kind: stateVerification, verification: event})
	return d.verificationDecision, err
}

// Pointer identity fences each check independently of public operation history.
type verificationAttempt struct{ operation *models.RecheckOperation }
