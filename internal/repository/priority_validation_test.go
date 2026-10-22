package repository

import (
	"errors"
	"testing"
	"torrent-stream-hub/internal/models"
)

func TestPriorityTargetValidation(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	r := NewTorrentRepo(db)
	if err := r.UpdateFilePriority("missing", 0, 0); !errors.Is(err, ErrTorrentNotFound) {
		t.Fatal(err)
	}
	if err := r.UpdateTorrentFilesPriority("missing", 0); !errors.Is(err, ErrTorrentNotFound) {
		t.Fatal(err)
	}
	if err := r.SaveTorrent(&models.Torrent{Hash: "pending"}); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateFilePriority("pending", 0, 0); !errors.Is(err, ErrMetadataPending) {
		t.Fatal(err)
	}
	if err := r.UpdateTorrentFilesPriority("pending", 0); !errors.Is(err, ErrMetadataPending) {
		t.Fatal(err)
	}
	if err := r.SaveTorrent(&models.Torrent{Hash: "known", Files: []*models.File{{Index: 0}}}); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateFilePriority("known", 1, 0); !errors.Is(err, ErrFileNotFound) {
		t.Fatal(err)
	}
	for range 2 {
		if err := r.UpdateFilePriority("known", 0, models.PriorityNone); err != nil {
			t.Fatal("idempotent update", err)
		}
	}
	if err := r.UpdateFilePriority("known", -1, 0); !errors.Is(err, ErrInvalidPriority) {
		t.Fatal(err)
	}
	if err := r.UpdateTorrentFilesPriority("known", 42); !errors.Is(err, ErrInvalidPriority) {
		t.Fatal(err)
	}
}
