package repository

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"torrent-stream-hub/internal/fastresume"
	"torrent-stream-hub/internal/models"
)

func TestResumeOwnerScanIsBoundedAndDoesNotCreateHeads(t *testing.T) {
	db, r, _, _, _ := resumeFixture(t)
	for i := 0; i < 130; i++ {
		hash := fmt.Sprintf("owner-%03d", i)
		if err := r.SaveTorrent(&models.Torrent{Hash: hash, State: models.StatePaused}); err != nil {
			t.Fatal(err)
		}
		if err := r.EnsureStorageMapping(&StorageMapping{Hash: hash, Version: 2, Root: filepath.Join("payload", hash), State: "ready"}); err != nil {
			t.Fatal(err)
		}
	}
	var before int
	if err := db.DB().QueryRow(`SELECT count(*) FROM resume_owners`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	cursor := ""
	for {
		owners, err := r.ResumeOwnersAfter(context.Background(), cursor)
		if err != nil || len(owners) > fastresume.MaxPendingOwners {
			t.Fatal(len(owners), err)
		}
		for _, owner := range owners {
			if owner <= cursor || seen[owner] {
				t.Fatal("cursor repeated/skipped ordering", owner, cursor)
			}
			seen[owner] = true
			cursor = owner
		}
		if len(owners) < fastresume.MaxPendingOwners {
			break
		}
	}
	if len(seen) != 131 {
		t.Fatal("lost owners across pages", len(seen))
	}
	var after int
	if err := db.DB().QueryRow(`SELECT count(*) FROM resume_owners`).Scan(&after); err != nil || after != before {
		t.Fatal("scan changed trust metadata", before, after, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.ResumeOwnersAfter(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestResumeAdmissionDoesNotCreateOrConsumeCheckpoint(t *testing.T) {
	_, r, m, c, token := resumeFixture(t)
	ctx := context.Background()
	if err := r.CheckResumeAdmission(ctx, m, c.Identity.RecordID); err != nil {
		t.Fatal(err)
	}
	token = commitResumeFixture(t, r, m, c, token)
	if err := r.CheckResumeAdmission(ctx, m, c.Identity.RecordID); err != nil {
		t.Fatal(err)
	}
	saved, err := r.LoadResume(ctx, m, c.Identity.RecordID)
	if err != nil || saved.Token != token || saved.Invalidated {
		t.Fatal(saved, err)
	}
	stale := *m
	stale.Revision++
	if err := r.CheckResumeAdmission(ctx, &stale, c.Identity.RecordID); !errors.Is(err, ErrStateConflict) {
		t.Fatal("stale mapping admitted", err)
	}
	if err := r.CheckResumeAdmission(ctx, m, "readded-record"); !errors.Is(err, ErrStateConflict) {
		t.Fatal("foreign record admitted", err)
	}
}
