package engine

import (
	"fmt"
	"math"

	"github.com/anacrolix/torrent"
	"torrent-stream-hub/internal/models"
)

// The persistence callback must not acquire application/library locks. It owns
// only the verification blocker, not operation history or any other safety key.
func (e *Engine) SetVerificationPersistence(save func(models.RuntimeFence, bool, string) error) {
	e.admissionMu.Lock()
	defer e.admissionMu.Unlock()
	e.verificationPersistence = save
}

func (e *Engine) persistVerificationLocked(mt *ManagedTorrent, uncertain bool, reason string) error {
	if e.verificationPersistence == nil {
		return nil
	}
	mt.mu.Lock()
	f := mt.runtimeFence
	mt.mu.Unlock()
	return e.verificationPersistence(f, uncertain, reason)
}

// RestoreVerificationBlock installs the persisted safety key before application
// restore releases command handles. It does not clear other errors or start a verifier.
func (e *Engine) RestoreVerificationBlock(hash string) error {
	e.admissionMu.Lock()
	defer e.admissionMu.Unlock()
	e.mu.RLock()
	mt := e.managedTorrents[hash]
	e.mu.RUnlock()
	if mt == nil {
		return TorrentNotFoundError{Hash: hash}
	}
	mt.mu.Lock()
	_, err := mt.applyVerificationEventLocked(verificationRestored)
	mt.mu.Unlock()
	if err != nil {
		return err
	}
	mt.t.DisallowDataDownload()
	mt.t.DisallowDataUpload()
	return nil
}

// RuntimeBinding returns identity and fence together. The caller must preserve
// expected across persistence; looking up by hash again would relabel a re-add.
func (e *Engine) RuntimeBinding(hash string) (expected *torrent.Torrent, fence models.RuntimeFence) {
	expected, fence, _ = e.RuntimeAppliedBinding(hash)
	return
}

func (e *Engine) RuntimeAppliedBinding(hash string) (expected *torrent.Torrent, fence models.RuntimeFence, applied int64) {
	e.mu.RLock()
	mt := e.managedTorrents[hash]
	e.mu.RUnlock()
	if mt == nil {
		return nil, fence, 0
	}
	mt.mu.Lock()
	defer mt.mu.Unlock()
	return mt.t, mt.runtimeFence, mt.appliedIntentRevision
}

// BindRuntime is called only after the application has committed and applied
// the specified intent. First attachment of legacy staged paths is explicitly
// coordinated by the application's record/root command handles.
func (e *Engine) BindRuntime(expected *torrent.Torrent, fence models.RuntimeFence) error {
	return e.bindRuntime(expected, fence, fence.Revision)
}

// BindObservedRuntime bootstraps observation delivery from legacy runtime
// paths. It does not assert that the saved command was applied to this runtime.
func (e *Engine) BindObservedRuntime(expected *torrent.Torrent, fence models.RuntimeFence) error {
	return e.bindRuntime(expected, fence, 0)
}

func (e *Engine) bindRuntime(expected *torrent.Torrent, fence models.RuntimeFence, applied int64) error {
	if expected == nil || fence.Attachment == "" || fence.RecordID == "" || fence.Revision < 1 {
		return fmt.Errorf("invalid runtime binding")
	}
	e.admissionMu.Lock()
	defer e.admissionMu.Unlock()
	if e.ctx.Err() != nil {
		return e.ctx.Err()
	}
	e.mu.RLock()
	mt := e.managedTorrents[fence.Hash]
	e.mu.RUnlock()
	if mt == nil || mt.t != expected {
		return ErrOperationConflict
	}
	mt.mu.Lock()
	old := mt.runtimeFence
	if applied > 0 && mt.initializing {
		mt.mu.Unlock()
		return ErrOperationConflict
	}
	if old.RecordID != "" && (old.RecordID != fence.RecordID || old.Revision > fence.Revision) {
		mt.mu.Unlock()
		return ErrOperationConflict
	}
	uncertain := mt.verificationUncertain
	blocks := make(map[models.ActivationBlockKind]string, len(mt.safetyBlocks))
	for k, v := range mt.safetyBlocks {
		blocks[k] = v
	}
	mt.mu.Unlock()
	// A command may commit its new revision while an automatic verifier is
	// waiting on the old fence. Rebinding must carry safety uncertainty forward,
	// never silently acknowledge a new attachment without its durable block.
	if uncertain && e.verificationPersistence != nil {
		if err := e.verificationPersistence(fence, true, "Verification incomplete; successful local check required"); err != nil {
			return err
		}
	}
	if e.safetyPersistence != nil {
		for k, v := range blocks {
			if err := e.safetyPersistence(fence, k, v); err != nil {
				return err
			}
		}
	}
	mt.mu.Lock()
	defer mt.mu.Unlock()
	mt.runtimeFence = fence
	mt.observationSequence = 0
	mt.appliedIntentRevision = applied
	if applied > 0 {
		mt.intentApplyFailure = nil
	}
	return nil
}

// FailIntentApply closes admission before publishing a partial command failure.
// expected is captured before the command: late failure must not gate a re-add.
// This does not mutate intent or remove any durable safety block. A full explicit
// Pause/Resume (or reopen from SQLite) is required to reconcile the runtime.
func (e *Engine) FailIntentApply(expected *torrent.Torrent, v models.IntentVersion, code string) error {
	if expected == nil || v.RecordID == "" || v.Revision < 1 {
		return ErrOperationConflict
	}
	switch code {
	case "intent_commit_failed", "runtime_stage_failed", "runtime_apply_failed", "runtime_bind_failed":
	default:
		return fmt.Errorf("invalid intent failure code")
	}
	e.admissionMu.Lock()
	defer e.admissionMu.Unlock()
	e.mu.RLock()
	mt := e.managedTorrents[v.Hash]
	e.mu.RUnlock()
	if mt == nil || mt.t != expected {
		return ErrOperationConflict
	}
	mt.mu.Lock()
	if (mt.runtimeFence.RecordID != "" && mt.runtimeFence.RecordID != v.RecordID) || mt.runtimeFence.Revision > v.Revision || (mt.intentApplyFailure != nil && mt.intentApplyFailure.Revision > v.Revision) {
		mt.mu.Unlock()
		return ErrOperationConflict
	}
	d, err := mt.applyStateEventLocked(stateEvent{kind: stateApplyFailed})
	if err != nil {
		mt.mu.Unlock()
		return err
	}
	// Fail-closed quiesce changed runtime facts; even a previously equal revision
	// is no longer proof that all axes of that saved intent remain applied.
	mt.intentApplyFailure = &models.IntentApplyFailure{RecordID: v.RecordID, Revision: v.Revision, Code: code, Retry: "explicit_pause_or_resume"}
	mt.mu.Unlock()
	e.applyQuiesceLocked(mt, d, "intent reconciliation required")
	return nil
}

// CaptureObservation uses the existing admission serialization until A04
// replaces it. Fence and sequence travel with the captured facts, not a later
// repository lookup. Ordinary REST/health reads never advance this sequence.
func (e *Engine) CaptureObservation(hash string) (*models.Torrent, models.RuntimeObservation, error) {
	e.mu.RLock()
	expected := e.managedTorrents[hash]
	e.mu.RUnlock()
	if expected == nil {
		return nil, models.RuntimeObservation{}, TorrentNotFoundError{Hash: hash}
	}
	expected.t.RefreshStorageCompletion()
	e.admissionMu.Lock()
	defer e.admissionMu.Unlock()
	e.mu.RLock()
	mt := e.managedTorrents[hash]
	e.mu.RUnlock()
	if mt == nil {
		return nil, models.RuntimeObservation{}, TorrentNotFoundError{Hash: hash}
	}
	if mt != expected {
		return nil, models.RuntimeObservation{}, ErrOperationConflict
	}
	mt.mu.Lock()
	if mt.runtimeFence.Attachment == "" || mt.observationSequence == math.MaxInt64 {
		mt.mu.Unlock()
		return nil, models.RuntimeObservation{}, ErrOperationConflict
	}
	mt.observationSequence++
	fence, seq, applied := mt.runtimeFence, mt.observationSequence, mt.appliedIntentRevision
	mt.mu.Unlock()
	t := e.mapManagedTorrent(mt)
	o := models.RuntimeObservation{Fence: fence, Sequence: seq, AppliedRevision: applied, State: t.State, Error: t.Error, ProgressSource: t.ProgressSource, ObservedAt: t.ProgressObservedAt, VerifiedBytes: t.Downloaded}
	for _, f := range t.Files {
		o.Files = append(o.Files, models.FileObservation{Index: f.Index, VerifiedBytes: f.Downloaded})
	}
	return t, o, nil
}
