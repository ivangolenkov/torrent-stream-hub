package repository

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/anacrolix/torrent/storage"
)

func TestResumeMutationPendingSealFence(t *testing.T) {
	_, r, m, c, token := resumeFixture(t)
	if err := r.InvalidateResumeMutation(context.Background(), m, token.RecordID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CommitResume(context.Background(), m, token, c); !errors.Is(err, ErrStateConflict) {
		t.Fatal("invalidated head admitted old pending seal", err)
	}
}

func TestResumeMutationNamespaceDuringWorkflow(t *testing.T) {
	for _, workflow := range []string{"journal", "deletion", "mapping-state"} {
		t.Run(workflow, func(t *testing.T) {
			db, r, m, c, token := resumeFixture(t)
			token = commitResumeFixture(t, r, m, c, token)
			var err error
			switch workflow {
			case "journal":
				_, err = db.DB().Exec(`INSERT INTO storage_recovery(hash,record_id,reason,stage) VALUES(?,?,'storage_move','copying')`, m.Hash, token.RecordID)
			case "deletion":
				_, err = db.DB().Exec(`UPDATE torrents SET deletion_mode='payload' WHERE hash=?`, m.Hash)
			case "mapping-state":
				_, err = db.DB().Exec(`UPDATE torrent_storage SET state='moving',manifest='{}' WHERE hash=?`, m.Hash)
			}
			if err != nil {
				t.Fatal(err)
			}
			var invalid bool
			if err := db.DB().QueryRow(`SELECT invalidated FROM resume_owners WHERE storage_id=?`, m.StorageID).Scan(&invalid); err != nil || !invalid {
				t.Fatal("workflow did not atomically revoke", invalid, err)
			}
			if err := r.InvalidateResumeMutation(context.Background(), m, token.RecordID); err != nil {
				t.Fatal("workflow retry cannot enforce namespace barrier", err)
			}
			c.Generation++
			if _, err := r.CommitResume(context.Background(), m, token, c); !errors.Is(err, ErrStateConflict) {
				t.Fatal("stale workflow seal survived", err)
			}
		})
	}
}

func TestResumeMutationHooksPolicyAndStaleCallbacks(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(map[bool]string{false: "fast", true: "verify"}[strict], func(t *testing.T) {
			_, r, m, c, token := resumeFixture(t)
			token = commitResumeFixture(t, r, m, c, token)
			hooks, err := r.NewResumeMutationHooks(context.Background(), m, token.RecordID, 3, 4, strict)
			if err != nil {
				t.Fatal(err)
			}
			want := byte(7)
			if strict {
				want = 0
			}
			if !bytes.Equal(hooks.Pending, []byte{want}) {
				t.Fatal("wrong pending trust", hooks.Pending)
			}
			if !strict {
				if discarded, err := hooks.Revoke([]byte{2}); err != nil || discarded {
					t.Fatal(err)
				}
				s, err := r.LoadResume(context.Background(), m, token.RecordID)
				if err != nil || !bytes.Equal(s.Revoked, []byte{2}) {
					t.Fatal("piece revoke not durable", s, err)
				}
			}
			stale := *m
			stale.Revision++
			if err := r.InvalidateResumeMutation(context.Background(), &stale, token.RecordID); !errors.Is(err, ErrStateConflict) {
				t.Fatal("stale mapping mutation admitted", err)
			}
			if err := r.InvalidateResumeMutation(context.Background(), m, "replaced-record"); !errors.Is(err, ErrStateConflict) {
				t.Fatal("stale record mutation admitted", err)
			}
		})
	}
}

func TestResumeMutationUnboundRegistration(t *testing.T) {
	db, r, m, c, token := resumeFixture(t)
	if safe, err := r.ResumeUnboundMutationSafe(context.Background(), m); err != nil || safe {
		t.Fatal("bound record treated as unbound", safe, err)
	}
	token = commitResumeFixture(t, r, m, c, token)
	if _, err := db.DB().Exec(`DELETE FROM torrent_intents WHERE hash=?`, m.Hash); err != nil {
		t.Fatal(err)
	}
	if safe, err := r.ResumeUnboundMutationSafe(context.Background(), m); err != nil || !safe {
		t.Fatal("invalidated re-add mapping not admitted", safe, err)
	}
	// Simulate corrupt authority left live: never admit an unbound writer.
	if _, err := db.DB().Exec(`UPDATE resume_owners SET invalidated=0 WHERE storage_id=?`, m.StorageID); err != nil {
		t.Fatal(err)
	}
	if safe, err := r.ResumeUnboundMutationSafe(context.Background(), m); err != nil || safe {
		t.Fatal("unowned live checkpoint granted mutation", safe, err)
	}
}

func TestResumeAdapterMissingCheckpointIsFallback(t *testing.T) {
	r, m, source, _, _ := openResumeLibrary(t, t.TempDir(), "v1", false)
	backend := storage.TorrentImpl{PrepareRestore: func(context.Context, storage.FileCheckpoint, func(context.Context) error) (*storage.RestoreToken, error) {
		t.Error("missing checkpoint reached import")
		return nil, nil
	}, RestoreCheckpoint: func(context.Context, *storage.RestoreToken) error { t.Error("missing checkpoint restored"); return nil }}
	if err := r.RestoreResume(context.Background(), m, source, backend); !errors.Is(err, ErrStateConflict) {
		t.Fatal(err)
	}
}
