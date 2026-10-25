package repository

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
)

func TestStorageOperationClaimsCommitAndRetainedOwnership(t *testing.T) {
	db, err := NewSQLiteDB(filepath.Join(t.TempDir(), "operations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewTorrentRepo(db)
	if err := r.SaveTorrent(&models.Torrent{Hash: "owner", Name: "Example", State: models.StatePaused}); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureStorageMapping(&StorageMapping{Hash: "owner", Version: 2, Root: filepath.FromSlash("payload/Old"), State: "ready"}); err != nil {
		t.Fatal(err)
	}
	if err := r.ClaimStorageAlias("alias", "owner"); err != nil {
		t.Fatal(err)
	}
	m, _ := r.StorageMapping("owner")
	intent, _ := r.GetIntent("owner")
	v := &RecoveryRecord{Hash: "owner", RecordID: intent.Version.RecordID, Version: 1, Reason: models.StorageMove, Stage: "planned", Journal: "{}"}
	claims := []StorageClaim{{StorageID: m.StorageID, RecordID: v.RecordID, Version: 1, Path: m.Root, Kind: "source"}, {StorageID: m.StorageID, RecordID: v.RecordID, Version: 1, Path: filepath.FromSlash("payload/New"), Kind: "target"}}
	if err := r.CreateStorageOperation(context.Background(), m, v, 0, claims); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureStorageMapping(&StorageMapping{Hash: "outsider", Version: 2, State: "reserved"}); err != nil {
		t.Fatal(err)
	}
	other, _ := r.StorageMapping("outsider")
	if err := r.SetStorageLayout(context.Background(), other, "", payload.Layout{Version: 2, Root: filepath.FromSlash("payload/new/child")}); !errors.Is(err, ErrStateConflict) {
		t.Fatal("claim not enforced", err)
	}
	v.Stage = "publishing"
	if err := r.TransitionStorageOperation(m, v, "planned"); err != nil {
		t.Fatal(err)
	}
	stale := *v
	stale.Stage = "cancel_requested"
	if err := r.TransitionStorageOperation(m, &stale, "planned"); !errors.Is(err, ErrStateConflict) {
		t.Fatal("late cancellation won", err)
	}
	v.Stage = "published"
	if err := r.TransitionStorageOperation(m, v, "publishing"); err != nil {
		t.Fatal(err)
	}
	next := payload.Layout{Version: 2, Root: filepath.FromSlash("payload/New")}
	if err := r.CommitStorageOperation(context.Background(), m, v, next, false); err != nil {
		t.Fatal(err)
	}
	after, _ := r.StorageMapping("owner")
	alias, _ := r.StorageMapping("alias")
	rec, _ := r.Recovery("owner")
	if after.Root != next.Root || alias.Root != next.Root || after.Revision != m.Revision+1 || rec.Stage != "committed" {
		t.Fatal("non-atomic commit", after, alias, rec)
	}
	v.Stage = "completed"
	if err := r.FinishStorageOperation(after, v, "committed", []string{m.Root}); err != nil {
		t.Fatal(err)
	}
	if err := r.BeginForget("owner"); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteTorrent("owner"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetStorageLayout(context.Background(), other, "", payload.Layout{Version: 2, Root: m.Root}); !errors.Is(err, ErrStateConflict) {
		t.Fatal("retained source lost ownership", err)
	}
	if err := r.CommitStorageOperation(context.Background(), m, v, next, false); !errors.Is(err, ErrStateConflict) {
		t.Fatal("late worker crossed deletion", err)
	}
}

func TestStorageOperationQueueIsBounded(t *testing.T) {
	db, err := NewSQLiteDB(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewTorrentRepo(db)
	for index := 0; index < 65; index++ {
		hash := fmt.Sprintf("owner-%d", index)
		if err := r.SaveTorrent(&models.Torrent{Hash: hash, State: models.StatePaused}); err != nil {
			t.Fatal(err)
		}
		if err := r.EnsureStorageMapping(&StorageMapping{Hash: hash, Version: 2, Root: filepath.Join("payload", hash), State: "ready"}); err != nil {
			t.Fatal(err)
		}
		m, _ := r.StorageMapping(hash)
		intent, _ := r.GetIntent(hash)
		v := &RecoveryRecord{Hash: hash, RecordID: intent.Version.RecordID, Version: 1, Reason: models.StorageMove, Stage: "queued", Journal: "{}"}
		err := r.CreateStorageOperation(context.Background(), m, v, 0, nil)
		if index < 64 && err != nil {
			t.Fatal(index, err)
		}
		if index == 64 && !errors.Is(err, ErrStorageQueueFull) {
			t.Fatal("unbounded queue", err)
		}
	}
	jobs, err := r.PendingStorageOperations(context.Background())
	if err != nil || len(jobs) != 64 {
		t.Fatal(len(jobs), err)
	}
}
