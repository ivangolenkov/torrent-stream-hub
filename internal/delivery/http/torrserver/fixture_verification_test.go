package torrserver

import (
	"errors"
	"testing"
	"time"
	"torrent-stream-hub/internal/usecase"
)

func verifyHTTPFixture(t testing.TB, uc *usecase.TorrentUseCase, hash string) {
	t.Helper()
	if _, err := uc.Recheck(hash); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		v, err := uc.GetTorrent(hash)
		if err != nil {
			if errors.Is(err, usecase.ErrSnapshotConflict) && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
				continue
			}
			t.Fatal(err)
		}
		if v.Recheck != nil {
			if v.Recheck.Status == "succeeded" {
				return
			}
			if v.Recheck.Status == "failed" {
				t.Fatal(v.Recheck.Error)
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture verification timeout", v.Recheck)
		}
		time.Sleep(time.Millisecond)
	}
}
