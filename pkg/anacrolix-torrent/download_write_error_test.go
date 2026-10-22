package torrent

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/anacrolix/torrent/internal/testutil"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

type enospcStorage struct{ storage.ClientImpl }

func (s enospcStorage) OpenTorrent(ctx context.Context, info *metainfo.Info, hash metainfo.Hash) (storage.TorrentImpl, error) {
	t, err := s.ClientImpl.OpenTorrent(ctx, info, hash)
	if err != nil {
		return t, err
	}
	piece := t.Piece
	t.Piece = func(p metainfo.Piece) storage.PieceImpl { return enospcPiece{piece(p)} }
	return t, nil
}

type enospcPiece struct{ storage.PieceImpl }

func (enospcPiece) WriteAt([]byte, int64) (int, error) { return 0, syscall.ENOSPC }

func TestWriteErrorSynchronouslyClosesAdmissionWithObserver(t *testing.T) {
	backend := storage.NewFile(t.TempDir())
	defer backend.Close()
	cfg := TestingConfig(t)
	cfg.DefaultStorage = enospcStorage{backend}
	cl, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	tor, err := cl.AddTorrent(testutil.GreetingMetaInfo())
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan error, 1), make(chan struct{})
	tor.SetOnWriteChunkError(func(err error) { entered <- err; <-release })
	defer close(release)
	err = tor.writeChunk(0, 0, []byte("x"))
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("fault writer: %v", err)
	}
	cl.lock()
	tor.onWriteChunkErr(err)
	cl.unlock()
	select {
	case err := <-entered:
		if !errors.Is(err, syscall.ENOSPC) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("observer not called")
	}
	// Observer is deliberately blocked: the library gate must already be closed.
	if tor.DataDownloadAllowed() {
		t.Fatal("async observer left requests enabled")
	}
	cl.lock()
	uploadDisabled := tor.dataUploadDisallowed
	cl.unlock()
	if uploadDisabled {
		t.Fatal("payload write error disabled verified seeding")
	}
	if err := tor.AllowDataDownloadIfNoWriteError(); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("automatic admission bypassed latch: %v", err)
	}
	reader := tor.NewReader()
	reader.SetResponsive()
	defer reader.Close()
	if tor.DataDownloadAllowed() {
		t.Fatal("responsive reader reopened admission")
	}
	version := tor.WriteChunkErrorVersion()
	cl.lock()
	tor.onWriteChunkErr(syscall.ENOSPC)
	cl.unlock()
	if err := tor.ClearWriteChunkErrorIfVersion(version); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("late error erased: %v", err)
	}
	if err := tor.ClearWriteChunkErrorIfVersion(tor.WriteChunkErrorVersion()); err != nil {
		t.Fatal(err)
	}
	if err := tor.AllowDataDownloadIfNoWriteError(); err != nil {
		t.Fatal(err)
	}
	if !tor.DataDownloadAllowed() {
		t.Fatal("explicit recovery failed")
	}
}

func TestInitialAdmissionOptionsApplyBeforeMetadata(t *testing.T) {
	cl := newTestingClient(t)
	spec := TorrentSpecFromMetaInfo(testutil.GreetingMetaInfo())
	spec.DisallowDataDownload = true
	spec.DisallowDataUpload = true
	tor, _, err := cl.AddTorrentSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	if tor.DataDownloadAllowed() {
		t.Fatal("initial prohibition ignored")
	}
	cl.lock()
	uploadDisabled := tor.dataUploadDisallowed
	cl.unlock()
	if !uploadDisabled {
		t.Fatal("initial upload prohibition ignored")
	}
	reader := tor.NewReader()
	defer reader.Close()
	reader.SetResponsive()
	if tor.DataDownloadAllowed() {
		t.Fatal("reader overrides initial prohibition")
	}
}
