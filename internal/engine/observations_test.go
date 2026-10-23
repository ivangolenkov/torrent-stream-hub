package engine

import (
	"errors"
	"testing"

	"torrent-stream-hub/internal/models"
)

func TestObservationBindingTracksAppliedRevisionAndRejectsOldPointer(t *testing.T) {
	e, mt := gateFixture(t)
	f := models.RuntimeFence{IntentVersion: models.IntentVersion{Hash: mt.hash, RecordID: "record", Revision: 1}, Attachment: "attachment"}
	if err := e.BindObservedRuntime(mt.t, f); err != nil {
		t.Fatal(err)
	}
	_, first, err := e.CaptureObservation(mt.hash)
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		e.GetTorrent(mt.hash)
		e.GetAllTorrents()
	}
	_, second, err := e.CaptureObservation(mt.hash)
	if err != nil {
		t.Fatal(err)
	}
	if first.Sequence != 1 || second.Sequence != 2 || first.AppliedRevision != 0 {
		t.Fatal(first, second)
	}
	f.Revision = 2
	f.Attachment = "applied"
	if err := e.BindRuntime(mt.t, f); err != nil {
		t.Fatal(err)
	}
	_, applied, err := e.CaptureObservation(mt.hash)
	if err != nil || applied.AppliedRevision != 2 || applied.Sequence != 1 {
		t.Fatal(applied, err)
	}
	if err := e.Delete(mt.hash); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddInfoHash(mt.hash); err != nil {
		t.Fatal(err)
	}
	f.Attachment = "late attachment"
	if err := e.BindRuntime(mt.t, f); !errors.Is(err, ErrOperationConflict) {
		t.Fatal("bound old pointer to readd", err)
	}
	_, fence := e.RuntimeBinding(mt.hash)
	if fence.Attachment != "" {
		t.Fatal("late binding changed new generation", fence)
	}
}

func TestStagedAddNeverAdmitsPayloadBeforeFinish(t *testing.T) {
	e, _ := gateFixture(t)
	hash := "7777777777777777777777777777777777777777"
	tor, err := e.AddStagedMagnet("magnet:?xt=urn:btih:" + hash)
	if err != nil {
		t.Fatal(err)
	}
	raw := e.GetRawTorrent(hash)
	for range 10 {
		e.manageResources()
	}
	if raw == nil || raw.DataDownloadAllowed() || tor.State != models.StateQueued {
		t.Fatal("precommit add was admitted", tor)
	}
	if err := e.FinishRestore(hash, models.StateQueued); err != nil {
		t.Fatal(err)
	}
	// The fixture's existing torrent may hold the slot; eligibility, not immediate
	// background Downloading, is the contract after FinishRestore.
	e.mu.RLock()
	mt := e.managedTorrents[hash]
	e.mu.RUnlock()
	mt.mu.Lock()
	initializing := mt.initializing
	mt.mu.Unlock()
	if initializing {
		t.Fatal("committed Add remains staged")
	}
}

func TestRebindingCarriesVerificationUncertaintyBeforeAcknowledgingRevision(t *testing.T) {
	e, mt := gateFixture(t)
	f := models.RuntimeFence{IntentVersion: models.IntentVersion{Hash: mt.hash, RecordID: "record", Revision: 1}, Attachment: "first"}
	if err := e.BindRuntime(mt.t, f); err != nil {
		t.Fatal(err)
	}
	if err := e.RestoreVerificationBlock(mt.hash); err != nil {
		t.Fatal(err)
	}
	var saved models.RuntimeFence
	e.SetVerificationPersistence(func(got models.RuntimeFence, uncertain bool, _ string) error {
		if !uncertain {
			t.Fatal("rebind cleared safety key")
		}
		saved = got
		return nil
	})
	f.Revision = 2
	f.Attachment = "second"
	if err := e.BindRuntime(mt.t, f); err != nil {
		t.Fatal(err)
	}
	if saved != f {
		t.Fatal("new attachment lost durable uncertainty", saved)
	}
	_, o, err := e.CaptureObservation(mt.hash)
	if err != nil || o.ProgressSource == "verified" {
		t.Fatal("binding restored readiness", o, err)
	}
}

func TestIntentApplyFailureIsImmutableFencedAndSurvivesObservationBinding(t *testing.T) {
	e, mt := gateFixture(t)
	f := models.RuntimeFence{IntentVersion: models.IntentVersion{Hash: mt.hash, RecordID: "record", Revision: 2}, Attachment: "runtime"}
	if err := e.BindRuntime(mt.t, f); err != nil {
		t.Fatal(err)
	}
	if err := e.FailIntentApply(mt.t, f.IntentVersion, "runtime_bind_failed"); err != nil {
		t.Fatal(err)
	}
	view := e.GetTorrent(mt.hash)
	if view.Control.ApplyFailure == nil || view.Control.AppliedRevision != 0 || mt.t.DataDownloadAllowed() {
		t.Fatal(view)
	}
	mt.mu.Lock()
	reserved, initializing := mt.state == models.StateDownloading, mt.initializing
	mt.mu.Unlock()
	if reserved || !initializing {
		t.Fatal("failure stranded a slot or lost admission veto")
	}
	view.Control.ApplyFailure.Code = "tampered snapshot"
	f.Attachment = "observed"
	if err := e.BindObservedRuntime(mt.t, f); err != nil {
		t.Fatal(err)
	}
	if got := e.GetTorrent(mt.hash).Control.ApplyFailure; got == nil || got.Code != "runtime_bind_failed" {
		t.Fatal("observation binding cleared/mutated failure", got)
	}
	older := f.IntentVersion
	older.Revision--
	if err := e.FailIntentApply(mt.t, older, "intent_commit_failed"); !errors.Is(err, ErrOperationConflict) {
		t.Fatal("accepted obsolete failure", err)
	}
	if err := e.Delete(mt.hash); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddInfoHash(mt.hash); err != nil {
		t.Fatal(err)
	}
	if err := e.FailIntentApply(mt.t, f.IntentVersion, "runtime_apply_failed"); !errors.Is(err, ErrOperationConflict) {
		t.Fatal("old failure gated readd", err)
	}
	if got := e.GetTorrent(mt.hash).Control.ApplyFailure; got != nil {
		t.Fatal("readd inherited failure", got)
	}
}
