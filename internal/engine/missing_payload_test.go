package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"torrent-stream-hub/internal/models"
)

func TestMissingPayloadDoesNotClassifyOrdinaryIncompleteFiles(t *testing.T) {
	e, mt := gateFixture(t)
	if ready, _, err := e.CheckFileDirectReady(mt.hash, 0); err != nil || ready {
		t.Fatal(ready, err)
	}
	src, err := e.OpenStreamSource(context.Background(), mt.hash, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	src.Close()
	mt.mu.Lock()
	blocks := len(mt.safetyBlocks)
	mt.mu.Unlock()
	if blocks != 0 || e.GetTorrent(mt.hash).State == models.StateMissingFiles {
		t.Fatal("ordinary incomplete torrent became MissingFiles")
	}
}

func TestMissingPayloadDoesNotClassifyHashMismatchOrPartFile(t *testing.T) {
	for _, mutation := range []string{"hash-mismatch", "part-file"} {
		t.Run(mutation, func(t *testing.T) {
			e, tor := setupCompletedLifecycleTorrent(t)
			if err := e.Pause(tor.Hash); err != nil {
				t.Fatal(err)
			}
			if ready, _, err := e.CheckFileDirectReady(tor.Hash, 0); err != nil || !ready {
				t.Fatal(ready, err)
			}
			path := filepath.Join(e.DownloadDir(), "payload", tor.Hash, "lifecycle", "video.mp4")
			if mutation == "part-file" {
				if err := os.Rename(path, path+".part"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, bytes.Repeat([]byte("Y"), 2048), 0600); err != nil {
					t.Fatal(err)
				}
				if err := e.GetRawTorrent(tor.Hash).VerifyDataContext(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if ready, _, err := e.CheckFileDirectReady(tor.Hash, 0); err != nil || ready {
				t.Fatal("mutation remained directly ready", ready, err)
			}
			e.mu.RLock()
			mt := e.managedTorrents[tor.Hash]
			e.mu.RUnlock()
			mt.mu.Lock()
			blocks := len(mt.safetyBlocks)
			mt.mu.Unlock()
			if blocks != 0 || e.GetTorrent(tor.Hash).State == models.StateMissingFiles {
				t.Fatal("non-absence became MissingFiles")
			}
		})
	}
}
