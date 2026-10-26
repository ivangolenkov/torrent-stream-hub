package repository

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anacrolix/torrent/storage"
)

func TestResumeSealBusyAfterFlushKeepsOldGeneration(t *testing.T) {
	r, m, source, tor, _ := openResumeBoundaryFixture(t, t.TempDir(), false, nil)
	baseline := source
	baseline.Capture = func(ctx context.Context, publish func(storage.FileCheckpoint) error) error {
		return tor.WithFileCheckpoint(ctx, func(c storage.FileCheckpoint) error { c.Complete[0] = 1; return publish(c) })
	}
	if err := r.SealResume(context.Background(), m, baseline); err != nil {
		t.Fatal(err)
	}
	blockedAtPublish := false
	source.Capture = func(ctx context.Context, publish func(storage.FileCheckpoint) error) error {
		return tor.WithFileCheckpoint(ctx, func(c storage.FileCheckpoint) error {
			// A different real connection owns SQLite's writer AFTER payload/
			// namespace flush, immediately before the seal attempts its commit.
			blocker, err := r.db.BeginWrite()
			if err != nil {
				return err
			}
			defer blocker.Rollback()
			blockedAtPublish = true
			return publish(c)
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := r.SealResume(ctx, m, source); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("busy seal did not fail within budget", err)
	}
	if !blockedAtPublish {
		t.Fatal("deadline expired before real post-flush SQLite contention")
	}
	saved, err := r.LoadResume(context.Background(), m, source.Identity.RecordID)
	if err != nil || saved == nil || saved.Token.Generation != 1 || !bytes.Equal(saved.Checkpoint.Pieces, []byte{1}) {
		t.Fatal("busy writer replaced checkpoint", saved, err)
	}
}

func TestResumeSealLostCommitAcknowledgementStillRevokesNewSubset(t *testing.T) {
	r, m, source, tor, _ := openResumeBoundaryFixture(t, t.TempDir(), false, nil)
	baseline := source
	baseline.Capture = func(ctx context.Context, publish func(storage.FileCheckpoint) error) error {
		return tor.WithFileCheckpoint(ctx, func(c storage.FileCheckpoint) error { c.Complete[0] = 1; return publish(c) })
	}
	if err := r.SealResume(context.Background(), m, baseline); err != nil {
		t.Fatal(err)
	}
	lost := errors.New("injected lost commit acknowledgement")
	source.Capture = func(ctx context.Context, publish func(storage.FileCheckpoint) error) error {
		return tor.WithFileCheckpoint(ctx, func(c storage.FileCheckpoint) error {
			if err := publish(c); err != nil {
				return err
			}
			return lost // SQLite actually committed; native/backend did not receive success.
		})
	}
	if err := r.SealResume(context.Background(), m, source); !errors.Is(err, lost) {
		t.Fatal(err)
	}
	saved, err := r.LoadResume(context.Background(), m, source.Identity.RecordID)
	if err != nil || saved == nil || saved.Token.Generation != 2 || !bytes.Equal(saved.Checkpoint.Pieces, []byte{3}) {
		t.Fatal("actual WAL commit missing", saved, err)
	}
	// Piece 1 was absent from generation 1. The backend must retain the UNION
	// of possibly persisted masks, not only the last acknowledged generation.
	if _, err := tor.Piece(1).Storage().WriteAt([]byte("evil"), 0); err != nil {
		t.Fatal(err)
	}
	saved, err = r.LoadResume(context.Background(), m, source.Identity.RecordID)
	if err != nil || saved == nil || saved.Revoked[0]&2 == 0 {
		t.Fatal("newly persisted piece escaped durable revocation", saved, err)
	}
}
