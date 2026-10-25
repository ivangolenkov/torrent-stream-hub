package torrent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/internal/testutil"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

type blockedOpenStorage struct {
	storage.ClientImpl
	entered, release chan struct{}
	closed           atomic.Int32
}

func (s *blockedOpenStorage) OpenTorrent(ctx context.Context, info *metainfo.Info, hash metainfo.Hash) (storage.TorrentImpl, error) {
	close(s.entered)
	<-s.release // Deliberately models an uncancellable filesystem syscall.
	impl, err := s.ClientImpl.OpenTorrent(ctx, info, hash)
	if err == nil {
		original := impl.Close
		impl.Close = func() error {
			s.closed.Add(1)
			if original != nil {
				return original()
			}
			return nil
		}
	}
	return impl, err
}

func TestBlockedStorageOpenAllowsUnrelatedTorrentAndJoinsDrop(t *testing.T) {
	cl := newTestingClient(t)
	backend := storage.NewFile(t.TempDir())
	defer backend.Close()
	slow := &blockedOpenStorage{ClientImpl: backend, entered: make(chan struct{}), release: make(chan struct{})}
	spec := TorrentSpecFromMetaInfo(testutil.GreetingMetaInfo())
	spec.Storage = slow
	added := make(chan struct{})
	go func() { _, _, _ = cl.AddTorrentSpec(spec); close(added) }()
	<-slow.entered
	unrelated := make(chan struct{})
	go func() { _, _ = cl.AddTorrentInfoHash(metainfo.Hash{1}); _ = cl.Torrents(); close(unrelated) }()
	select {
	case <-unrelated:
	case <-time.After(time.Second):
		close(slow.release)
		t.Fatal("OpenTorrent holds client lock")
	}
	tor, ok := cl.Torrent(spec.InfoHash)
	if !ok {
		close(slow.release)
		t.Fatal("in-progress identity missing")
	}
	dropped := make(chan struct{})
	go func() { tor.Drop(); close(dropped) }()
	select {
	case <-tor.closed.Done():
	case <-time.After(time.Second):
		close(slow.release)
		t.Fatal("drop could not revoke open")
	}
	select {
	case <-dropped:
		close(slow.release)
		t.Fatal("drop returned before owned syscall")
	default:
	}
	close(slow.release)
	select {
	case <-dropped:
	case <-time.After(time.Second):
		t.Fatal("drop did not join open")
	}
	<-added
	if slow.closed.Load() != 1 || tor.Info() != nil {
		t.Fatal("late storage result attached or leaked")
	}
	if _, ok := cl.Torrent(spec.InfoHash); ok {
		t.Fatal("closed alias reinserted")
	}
}
