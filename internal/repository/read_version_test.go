package repository

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"torrent-stream-hub/internal/models"
)

func TestReadVersionIgnoresTelemetryAndSeparatesRecords(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	r := NewTorrentRepo(db)
	i := intentFixture(t, r, "a")
	other := intentFixture(t, r, "b")
	f, err := r.AttachRuntime(i.Version)
	if err != nil {
		t.Fatal(err)
	}
	o := verifiedObservation(f, 1, 4, 100)
	if err := r.SaveRuntimeObservation(o); err != nil {
		t.Fatal(err)
	}
	before, err := r.ReadVersion()
	if err != nil {
		t.Fatal(err)
	}
	av, _ := r.ReadVersion("a")
	o.Sequence++
	o.ObservedAt++
	o.VerifiedBytes = 6
	o.Files[0].VerifiedBytes = 6
	if err := r.SaveRuntimeObservation(o); err != nil {
		t.Fatal(err)
	}
	after, _ := r.ReadVersion()
	if before != after {
		t.Fatal("telemetry invalidated durable version")
	}
	if _, err := r.CommitAddMetadata(other.Version, models.UserMetadata{Title: "B"}, false); err != nil {
		t.Fatal(err)
	}
	av2, _ := r.ReadVersion("a")
	after, _ = r.ReadVersion()
	if av != av2 || before == after {
		t.Fatal("record and list versions not separated")
	}
	if err := r.DeleteTorrent("a"); err != nil {
		t.Fatal(err)
	}
	intentFixture(t, r, "a")
	av2, _ = r.ReadVersion("a")
	if av == av2 {
		t.Fatal("ABA retained read version")
	}
}

func TestObservationWriterWaitIsCancelled(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	r := NewTorrentRepo(db)
	i := intentFixture(t, r, "a")
	f, err := r.AttachRuntime(i.Version)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := db.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.SaveRuntimeObservation(verifiedObservation(f, 1, 4, 1), ctx) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("writer ignored cancellation")
	}
}

func TestReadModelWaitForPoolIsCancellable(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	r := NewTorrentRepo(db)
	db.DB().SetMaxOpenConns(1)
	held, err := db.DB().Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	for name, read := range map[string]func(context.Context) error{
		"version": func(ctx context.Context) error { _, err := r.ReadVersionContext(ctx); return err },
		"list":    func(ctx context.Context) error { _, err := r.GetAllTorrentsContext(ctx); return err },
		"record":  func(ctx context.Context) error { _, err := r.GetTorrentContext(ctx, "a"); return err },
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			before := db.DB().Stats().WaitCount
			done := make(chan error, 1)
			go func() { done <- read(ctx) }()
			deadline := time.After(time.Second)
			for db.DB().Stats().WaitCount == before {
				select {
				case <-deadline:
					t.Fatal("read did not wait for the occupied pool")
				default:
					runtime.Gosched()
				}
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("read model ignored cancellation")
			}
		})
	}
}
