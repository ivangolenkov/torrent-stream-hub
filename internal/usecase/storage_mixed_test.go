package usecase

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
)

func TestStorageMixedLibraryConvertsOnlyChosenTorrent(t *testing.T) {
	uc, repo, done := setupTorrentUseCase(t)
	defer done()
	chosen, old := moveFixture(t, uc, true, []byte("chosen"))
	neighbor, untouched := moveFixture(t, uc, true, []byte("neighbor"))
	newTorrent, named := moveFixture(t, uc, false, []byte("new"))
	if old.Version != 1 || untouched.Version != 1 || named.Version != payload.NamedLayoutVersion {
		t.Fatal("fixture is not mixed")
	}
	events := locationEvents(uc)
	cmd := planMoveFixture(t, uc, chosen, old, events)
	if _, err := uc.StorageCommand(context.Background(), chosen.Hash, cmd); err != nil {
		t.Fatal(err)
	}
	waitLocationEvent(t, uc, chosen.Hash, events, "location_completed")
	for _, item := range []struct {
		hash, content string
		revision      int64
	}{{neighbor.Hash, "neighbor", untouched.Revision}, {newTorrent.Hash, "new", named.Revision}} {
		mapping, err := repo.StorageMapping(item.hash)
		if err != nil || mapping.Revision != item.revision {
			t.Fatal("unrelated mapping changed", err)
		}
		path, err := mapping.Layout().Path(filepath.Join("Original", "video.bin"))
		if err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(filepath.Join(uc.engine.DownloadDir(), path))
		if err != nil || !bytes.Equal(content, []byte(item.content)) {
			t.Fatal("unrelated payload changed", err)
		}
		if ok, err := uc.restorePersistedTorrent(context.Background(), item.hash); err != nil || !ok {
			t.Fatal("mixed layout failed to reopen", ok, err)
		}
		if live := uc.engine.GetTorrent(item.hash); live == nil || live.State != models.StatePaused {
			t.Fatal("mixed restore resumed torrent")
		}
	}
	after, err := repo.StorageMapping(chosen.Hash)
	if err != nil || after.Version != 2 || after.Revision != old.Revision+1 {
		t.Fatal("chosen conversion failed", err)
	}
}
