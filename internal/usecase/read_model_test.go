package usecase

import (
	"encoding/json"
	"testing"
	"torrent-stream-hub/internal/models"
)

func TestReadModelSeparatesCommittedIntentFromAppliedRuntime(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor := temporaryFile(t, uc, "read-model", false)
	i, err := repo.GetIntent(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	before, err := uc.GetTorrent(tor.Hash)
	if err != nil || before.Control == nil || before.Control.ReconcilePending {
		t.Fatal(before, err)
	}
	if _, err := repo.CommitRunIntent(i.Version, models.RunPaused); err != nil {
		t.Fatal(err)
	}
	assertPending := func(tor *models.Torrent) {
		t.Helper()
		c := tor.Control
		if c == nil || c.DesiredRun != models.RunPaused || c.DesiredRevision != i.Version.Revision+1 || c.AppliedRevision != i.Version.Revision || !c.ReconcilePending || c.ReconcileReason != "intent_not_applied" {
			t.Fatal("lost commit/apply distinction", c)
		}
	}
	single, err := uc.GetTorrent(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	assertPending(single)
	list, err := uc.GetAllTorrents()
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	assertPending(list[0])
	sw := NewSyncWorker(uc)
	defer sw.Close()
	ch := make(chan []byte, 1)
	sw.AddClient(ch)
	defer sw.RemoveClient(ch)
	sw.Refresh()
	var snapshot []*models.Torrent
	if err := json.Unmarshal(<-ch, &snapshot); err != nil || len(snapshot) != 1 {
		t.Fatal(snapshot, err)
	}
	assertPending(snapshot[0])
	if err := uc.Pause(tor.Hash); err != nil {
		t.Fatal(err)
	}
	after, err := uc.GetTorrent(tor.Hash)
	if err != nil || after.Control.ReconcilePending || after.Control.AppliedRevision != i.Version.Revision+2 {
		t.Fatal(after, err)
	}
	if err := uc.engine.Delete(tor.Hash); err != nil {
		t.Fatal(err)
	}
	dbOnly, err := uc.GetTorrent(tor.Hash)
	if err != nil || dbOnly.Control.AppliedRevision != 0 || dbOnly.Control.RuntimeAttachment != "" || dbOnly.Control.ReconcileReason != "runtime_not_attached" {
		t.Fatal(dbOnly, err)
	}
}

func TestReadModelRejectsUncommittedAndOldRecordRuntime(t *testing.T) {
	old := &models.Torrent{Hash: "hash", Downloaded: 100, Control: &models.TorrentControl{RecordID: "old", AppliedRevision: 20, RuntimeAttachment: "old-runtime"}}
	if got := projectTorrent(old, nil); got != nil {
		t.Fatal("published uncommitted runtime", got)
	}
	current := &models.Torrent{Hash: "hash", Control: &models.TorrentControl{RecordID: "new", DesiredRevision: 1}}
	if got := projectTorrent(old, current); got != current || got.Downloaded != 0 {
		t.Fatal("old record contributed readiness", got)
	}
	if got := projectTorrents([]*models.Torrent{old}, nil); len(got) != 0 {
		t.Fatal("list resurrected runtime", got)
	}
}

func TestReadModelUsesPersistedMetadataAndBasePriorities(t *testing.T) {
	live := &models.Torrent{Hash: "hash", Title: "old", MetadataKnown: true, Files: []*models.File{{Index: 0, Priority: models.PriorityNormal}}, Control: &models.TorrentControl{RecordID: "record", RuntimeAttachment: "runtime", AppliedRevision: 1}}
	saved := &models.Torrent{Hash: "hash", Title: "new", Files: []*models.File{{Index: 0, Priority: models.PriorityNone}}, Control: &models.TorrentControl{RecordID: "record", DesiredRevision: 2, DesiredRun: models.RunPaused}}
	got := projectTorrent(live, saved)
	if got.Title != "new" || got.Files[0].Priority != models.PriorityNone || !got.Control.ReconcilePending {
		t.Fatal(got)
	}
}

func TestIncrementalCommandsCannotAcknowledgeEarlierUnappliedIntent(t *testing.T) {
	uc, repo, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor := temporaryFile(t, uc, "incremental-pending", false)
	i, err := repo.GetIntent(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CommitRunIntent(i.Version, models.RunPaused); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.UpdateMetadata(tor.Hash, TorrentMetadata{Title: "new title"}); err != nil {
		t.Fatal(err)
	}
	if err := uc.MakePermanent(tor.Hash); err != nil {
		t.Fatal(err)
	}
	if err := uc.SetFilePriority(tor.Hash, 0, models.PriorityNone); err != nil {
		t.Fatal(err)
	}
	model, err := uc.GetTorrent(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if model.Title != "new title" || model.IsTemporary || model.Files[0].Priority != models.PriorityNone || !model.Control.ReconcilePending || model.Control.AppliedRevision != i.Version.Revision {
		t.Fatal("incremental command acknowledged failed Pause", model)
	}
}
