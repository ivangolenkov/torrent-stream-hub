package usecase

import (
	"errors"
	"testing"

	"torrent-stream-hub/internal/models"
)

func TestConsistentReadRetriesCommandRevisionChange(t *testing.T) {
	uc, r, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor := temporaryFile(t, uc, "snapshot-command", false)
	captures := 0
	items, err := readConsistentTorrents(r.GetAllTorrents, func() []*models.Torrent {
		captures++
		old := uc.engine.GetAllTorrents()
		if captures == 1 {
			if err := uc.Pause(tor.Hash); err != nil {
				t.Fatal(err)
			}
		}
		return old
	}, func() func() bool {
		stamp := uc.engine.CaptureReadStamp("")
		return func() bool { return uc.engine.ValidateReadStamp(stamp) }
	})
	if err != nil || captures != 2 || len(items) != 1 || items[0].Control.DesiredRun != models.RunPaused || items[0].State != models.StatePaused {
		t.Fatal("mixed command/runtime snapshot escaped", items, captures, err)
	}
}

func TestConsistentReadRejectsRuntimeABAWithoutDatabaseChange(t *testing.T) {
	uc, r, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor := temporaryFile(t, uc, "snapshot-runtime-aba", false)
	captures := 0
	items, err := readConsistentTorrents(r.GetAllTorrents, func() []*models.Torrent {
		captures++
		old := uc.engine.GetAllTorrents()
		if captures == 1 {
			if err := uc.engine.Pause(tor.Hash); err != nil {
				t.Fatal(err)
			}
			if err := uc.engine.Resume(tor.Hash); err != nil {
				t.Fatal(err)
			}
		}
		return old
	}, func() func() bool {
		stamp := uc.engine.CaptureReadStamp("")
		return func() bool { return uc.engine.ValidateReadStamp(stamp) }
	})
	if err != nil || captures != 2 || len(items) != 1 {
		t.Fatal("phase ABA passed validation", captures, err)
	}
}

func TestConsistentReadRejectsOldRuntimeAfterRecordReadd(t *testing.T) {
	uc, r, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor := temporaryFile(t, uc, "snapshot-readd", false)
	oldID := tor.Control.RecordID
	captures := 0
	items, err := readConsistentTorrents(r.GetAllTorrents, func() []*models.Torrent {
		captures++
		old := uc.engine.GetAllTorrents()
		if captures == 1 {
			if err := r.DeleteTorrent(tor.Hash); err != nil {
				t.Fatal(err)
			}
			if err := r.CreateTorrent(models.NewTorrentRecord{Hash: tor.Hash, Name: tor.Name, Run: models.RunPaused}); err != nil {
				t.Fatal(err)
			}
		}
		return old
	}, func() func() bool {
		stamp := uc.engine.CaptureReadStamp("")
		return func() bool { return uc.engine.ValidateReadStamp(stamp) }
	})
	if err != nil || captures != 2 || len(items) != 1 || items[0].Control.RecordID == oldID || items[0].Control.AppliedRevision != 0 || items[0].State != models.StatePaused || items[0].Activity != (models.TorrentActivity{}) {
		t.Fatal("old runtime contributed to replacement", items, captures, err)
	}
}

func TestConsistentReadBoundsRetriesAndNeverReturnsEmptyOnChurnOrError(t *testing.T) {
	uc, r, cleanup := setupTorrentUseCase(t)
	defer cleanup()
	tor := temporaryFile(t, uc, "snapshot-churn", false)
	captures := 0
	items, err := readConsistentTorrents(r.GetAllTorrents, func() []*models.Torrent {
		captures++
		i, err := r.GetIntent(tor.Hash)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.UpdateUserMetadata(i.Version, models.UserMetadata{Title: "changed"}); err != nil {
			t.Fatal(err)
		}
		return uc.engine.GetAllTorrents()
	}, func() func() bool { return func() bool { return true } })
	if !errors.Is(err, ErrSnapshotConflict) || items != nil || captures != 2 {
		t.Fatal("unbounded retry or guessed empty snapshot", items, captures, err)
	}
	injected := errors.New("SQLite read failed")
	items, err = readConsistentTorrents(func() ([]*models.Torrent, error) { return nil, injected }, func() []*models.Torrent { t.Fatal("read runtime after database failure"); return nil }, func() func() bool { return func() bool { return true } })
	if !errors.Is(err, injected) || items != nil {
		t.Fatal(items, err)
	}
}
