package usecase

import "torrent-stream-hub/internal/models"

// restoreActivation projects durable authorities, never the last observation.
// Display precedence does not remove any key. In particular a paused record
// remains paused even when verification/disk/legacy safety keys coexist.
// All activation-block restores must remain offline regardless of display phase.
func restoreActivation(run models.RunIntent, disk string, blocks []models.ActivationBlock) (models.TorrentState, models.ErrorReason) {
	if run == models.RunPaused {
		return models.StatePaused, models.ErrNone
	}
	if run != models.RunEnabled {
		return models.StateError, models.ErrActivationBlocked
	}
	if disk != "" {
		return models.StateDiskFull, models.ErrorReason(disk)
	}
	// Explicit precedence, not SQL/arrival order or the spelling of reason text.
	for _, kind := range []models.ActivationBlockKind{models.BlockMissingFiles, models.BlockMetadataPersistence, models.BlockInvalidSelection, models.BlockLegacyError} {
		for _, b := range blocks {
			if b.Kind == kind {
				if kind == models.BlockMissingFiles {
					return models.StateMissingFiles, models.ErrMissingFiles
				}
				if kind == models.BlockMetadataPersistence {
					return models.StateError, models.ErrMetadataPersistence
				}
				if kind == models.BlockInvalidSelection {
					return models.StateError, models.ErrInvalidSelection
				}
				// Even legacy text equal to "Verification failed" is not owned by
				// the verifier and must not be cleared by its successful completion.
				return models.StateError, models.ErrActivationBlocked
			}
		}
	}
	for _, b := range blocks {
		if b.Kind != models.BlockVerification {
			// Unknown future keys fail closed, never as verification-owned errors.
			return models.StateError, models.ErrActivationBlocked
		}
	}
	if len(blocks) > 0 {
		return models.StateError, models.ErrVerificationFailed
	}
	return models.StateQueued, models.ErrNone
}
