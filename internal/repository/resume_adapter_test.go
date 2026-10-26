package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/anacrolix/torrent/storage"
)

func TestResumeAdapterRejectsLateSeal(t *testing.T) {
	for _, failure := range []string{"attachment", "epoch", "cancel", "capture", "catalog"} {
		t.Run(failure, func(t *testing.T) {
			r, m, source, tor, _ := openResumeLibrary(t, t.TempDir(), "v1", false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := r.SealResume(ctx, m, source); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("stale attachment/failed capture")
			current := true
			source.Authorize = func(ctx context.Context) error {
				if !current {
					return injected
				}
				return ctx.Err()
			}
			source.Capture = func(ctx context.Context, publish func(storage.FileCheckpoint) error) error {
				if failure == "capture" {
					return injected
				}
				return tor.WithFileCheckpoint(ctx, func(native storage.FileCheckpoint) error {
					switch failure {
					case "attachment":
						current = false
					case "cancel":
						cancel()
					case "catalog":
						native.Root = "other"
					case "epoch":
						saved, err := r.LoadResume(ctx, m, source.Identity.RecordID)
						if err != nil {
							return err
						}
						if _, err := r.InvalidateResume(ctx, m, saved.Token); err != nil {
							return err
						}
					}
					return publish(native)
				})
			}
			if err := r.SealResume(ctx, m, source); err == nil {
				t.Fatal("late seal succeeded")
			}
			saved, err := r.LoadResume(context.Background(), m, source.Identity.RecordID)
			if err != nil {
				t.Fatal(err)
			}
			if saved.Token.Generation != 1 || saved.Invalidated != (failure == "epoch") {
				t.Fatal("failed seal replaced or revived previous generation", saved)
			}
		})
	}
}

func TestResumeAdapterRejectsInvalidCapability(t *testing.T) {
	r, m, source, _, _ := openResumeLibrary(t, t.TempDir(), "v1", false)
	if _, err := r.NewResumeProducer(nil); err == nil {
		t.Fatal("nil lookup accepted")
	}
	for _, failure := range []string{"mapping", "metadata", "info", "authorize", "capture"} {
		t.Run(failure, func(t *testing.T) {
			s := source
			switch failure {
			case "mapping":
				s.Identity.MappingRevision++
			case "metadata":
				s.Metadata = nil
			case "info":
				s.Info = nil
			case "authorize":
				s.Authorize = nil
			case "capture":
				s.Capture = nil
			}
			if err := r.SealResume(context.Background(), m, s); err == nil {
				t.Fatal("invalid capability sealed")
			}
		})
	}
	for _, failure := range []string{"wrong-owner", "no-release", "lookup-error"} {
		t.Run(failure, func(t *testing.T) {
			var released bool
			p, err := r.NewResumeProducer(func(context.Context, string) (*StorageMapping, ResumeSource, func(), error) {
				release := func() { released = true }
				if failure == "no-release" {
					release = nil
				}
				if failure == "lookup-error" {
					return nil, ResumeSource{}, release, errors.New("lookup failed")
				}
				return m, source, release, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			owner := m.StorageID
			if failure == "wrong-owner" {
				owner = "other"
			}
			p.MarkDirty(owner)
			if err := p.Shutdown(context.Background()); err == nil {
				t.Fatal("invalid lookup sealed")
			}
			_ = p.Wait()
			if released != (failure != "no-release") {
				t.Fatal("lookup reservation lifetime broken")
			}
		})
	}
}
