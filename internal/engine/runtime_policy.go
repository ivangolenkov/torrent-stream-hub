package engine

import "torrent-stream-hub/internal/models"

// runtimePolicy owns applied runtime authority. phase/error are outputs, never
// inputs. The durable desired revision remains owned by SQLite; staging closes
// readiness without pretending an uncommitted command has been applied.
type runtimePolicy struct {
	selectionEmpty                                           bool
	registered, terminal                                     bool
	run                                                      models.RunIntent
	slot                                                     bool
	checking, uncertain, initializing                        bool
	verificationFault                                        bool
	disk                                                     string
	metadataFault, selectionFault, missingFault, legacyFault models.ErrorReason
	selectedComplete, payloadComplete                        bool
}

func (p runtimePolicy) fault() (models.TorrentState, models.ErrorReason) {
	switch {
	case p.missingFault != "":
		return models.StateMissingFiles, p.missingFault
	case p.metadataFault != "":
		return models.StateError, p.metadataFault
	case p.selectionFault != "":
		return models.StateError, p.selectionFault
	case p.legacyFault != "":
		return models.StateError, p.legacyFault
	default:
		return "", models.ErrNone
	}
}

func (p runtimePolicy) payloadEligible() bool {
	_, fault := p.fault()
	return p.registered && !p.terminal && p.run == models.RunEnabled && !p.initializing && !p.checking && !p.uncertain && !p.verificationFault && p.disk == "" && fault == models.ErrNone
}

func (p runtimePolicy) uploadEligible() bool {
	_, fault := p.fault()
	return p.registered && !p.terminal && p.run == models.RunEnabled && !p.initializing && !p.checking && !p.uncertain && !p.verificationFault && fault == models.ErrNone
}

func (p runtimePolicy) projection() (models.TorrentState, models.ErrorReason) {
	if p.run == models.RunPaused {
		return models.StatePaused, models.ErrNone
	}
	if p.disk != "" {
		return models.StateDiskFull, models.ErrorReason(p.disk)
	}
	if phase, err := p.fault(); phase != "" {
		return phase, err
	}
	if p.verificationFault && !p.checking {
		return models.StateError, models.ErrVerificationFailed
	}
	if !p.terminal && !p.initializing && !p.checking && !p.uncertain {
		if p.payloadComplete && !p.selectionEmpty {
			return models.StateSeeding, models.ErrNone
		}
		if p.slot {
			return models.StateDownloading, models.ErrNone
		}
	}
	return models.StateQueued, models.ErrNone
}

// runtimeTransition is pure. It never infers an intent, operation or reservation
// from a previous display phase. Compatibility phases enter only at registration
// and full activation, where validated persisted intent/blockers are supplied.
func runtimeTransition(p runtimePolicy, event stateEvent) (stateDecision, error) {
	reject := func(reason string) (stateDecision, error) {
		return stateDecision{}, StateTransitionError{Event: string(event.kind), Reason: reason}
	}
	if p.terminal {
		return reject("runtime_terminal")
	}
	if !p.registered && event.kind != stateRegistered {
		return reject("not_registered")
	}
	n := p
	d := stateDecision{}
	switch event.kind {
	case stateRegistered, stateActivated:
		switch event.phase {
		case models.StatePaused, models.StateQueued, models.StateDownloading, models.StateSeeding, models.StateStreaming, models.StateDiskFull, models.StateError, models.StateMissingFiles:
		default:
			return reject("unknown_activation_phase")
		}
		if event.kind == stateRegistered {
			if p.registered {
				return reject("already_registered")
			}
			n.registered = true
			n.initializing = event.staged
			n.disk = event.diskReason
		} else {
			if !p.registered {
				return reject("not_registered")
			}
			if p.checking || p.uncertain {
				return reject("verification_veto")
			}
			if p.initializing && !event.fullReconcile {
				return reject("initialization_veto")
			}
			n.initializing = false
			if event.diskRecovered {
				n.disk = ""
				n.metadataFault, n.selectionFault, n.missingFault, n.legacyFault = "", "", "", ""
			}
		}
		n.run = models.RunEnabled
		if event.phase == models.StatePaused {
			n.run = models.RunPaused
		}
		switch event.phase {
		case models.StateMissingFiles:
			n.missingFault = models.ErrMissingFiles
		case models.StateError:
			switch event.err {
			case models.ErrVerificationFailed:
				n.verificationFault = true
			case models.ErrMetadataPersistence:
				n.metadataFault = event.err
			case models.ErrInvalidSelection:
				n.selectionFault = event.err
			default:
				n.legacyFault = event.err
				if n.legacyFault == "" {
					n.legacyFault = models.ErrActivationBlocked
				}
			}
		case models.StateDiskFull:
			if n.disk == "" {
				n.disk = string(event.err)
				if n.disk == "" {
					n.disk = string(models.ErrDiskFull)
				}
			}
		}
		n.slot, n.payloadComplete = false, false
		d.resetDownload, d.closePayload = true, true
		d.allowUpload = n.uploadEligible()
		d.closeUpload = !d.allowUpload
	case stateDeleted, stateClosed:
		n.terminal, n.initializing = true, true
		if n.checking {
			n.checking, n.uncertain = false, true
		}
		n.slot = false
		d.resetDownload, d.closePayload, d.closeUpload, d.clearBasePriorities, d.invalidateApplied = true, true, true, true, true
	case statePaused:
		n.run, n.slot = models.RunPaused, false
		if event.fullReconcile {
			n.initializing = false
		}
		d.resetDownload, d.closePayload, d.closeUpload, d.clearBasePriorities, d.invalidateApplied = true, true, true, true, true
	case stateRestoreStaged:
		if p.checking || p.uncertain {
			return reject("verification_veto")
		}
		n.initializing, n.slot = true, false
		d.resetDownload, d.closePayload, d.closeUpload, d.clearBasePriorities, d.invalidateApplied = true, true, true, true, true
	case stateVerification:
		switch event.verification {
		case verificationBegin:
			if p.checking || p.initializing {
				return reject("verification_conflict")
			}
			n.checking, n.uncertain = true, true
			n.selectedComplete, n.payloadComplete = false, false
		case verificationSucceeded, verificationFailed, verificationCancelled:
			if !p.checking {
				return reject("verification_conflict")
			}
			n.checking = false
			n.uncertain = event.verification != verificationSucceeded
			n.verificationFault = event.verification != verificationSucceeded
		case verificationRestored:
			n.uncertain = true
		default:
			return reject("verification_conflict")
		}
		n.slot = false
		d.resetDownload, d.closePayload = true, true
		d.allowUpload = n.uploadEligible()
		d.closeUpload = !d.allowUpload
	case stateSelectionObserved:
		if !event.metadataKnown {
			return reject("metadata_unknown")
		}
		if event.payloadComplete && !event.selectedComplete {
			return reject("inconsistent_completion")
		}
		if p.initializing || p.checking || p.uncertain {
			return reject("runtime_veto")
		}
		n.selectedComplete, n.payloadComplete = event.selectedComplete, event.payloadComplete
		n.selectionEmpty = event.selectionEmpty
		if n.selectedComplete || !n.payloadEligible() {
			n.slot = false
			d.resetDownload = p.slot
			d.clearBasePriorities = p.slot
			d.closeBackgroundPayload = p.slot
		}
	case stateSlotRequested:
		if !p.payloadEligible() {
			return reject("runtime_veto")
		}
		if p.slot {
			return reject("slot_already_owned")
		}
		if p.selectedComplete || p.payloadComplete {
			return reject("selection_complete")
		}
		if !event.slotAvailable {
			return reject("capacity_unavailable")
		}
		n.slot = true
	case stateSlotReleased:
		if !p.slot {
			return reject("slot_not_owned")
		}
		n.slot = false
		d.resetDownload, d.clearBasePriorities, d.closeBackgroundPayload = true, true, true
	case stateDiskBlocked:
		if event.diskReason == "" {
			return reject("empty_disk_reason")
		}
		n.disk, n.slot = event.diskReason, false
		d.resetDownload, d.closePayload = true, true
	case stateApplyFailed, stateMetadataFailed, stateSelectionInvalid, statePayloadMissing:
		n.initializing, n.slot = true, false
		switch event.kind {
		case stateMetadataFailed:
			n.metadataFault = models.ErrMetadataPersistence
		case stateSelectionInvalid:
			n.selectionFault = models.ErrInvalidSelection
		case statePayloadMissing:
			n.missingFault = models.ErrMissingFiles
		}
		d.resetDownload, d.closePayload, d.closeUpload, d.clearBasePriorities, d.invalidateApplied = true, true, true, true, true
	default:
		return reject("unknown_event")
	}
	d.policy = n
	d.next.state, d.next.err = n.projection()
	d.next.checking, d.next.uncertain, d.next.initializing, d.next.diskBlock = n.checking, n.uncertain, n.initializing, n.disk
	d.selectionComplete, d.terminal = n.selectedComplete, n.terminal
	return d, nil
}
