package torrent

import (
	"bytes"
	"context"
	"crypto/sha1"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/anacrolix/missinggo/v2/bitmap"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

type verificationRequest struct {
	name    string
	piece   int
	proceed chan struct{}
}
type verificationBarrierStorage struct {
	storage.ClientImpl
	requests chan verificationRequest
	stop     chan struct{}
}

func (s verificationBarrierStorage) OpenTorrent(ctx context.Context, info *metainfo.Info, hash metainfo.Hash) (storage.TorrentImpl, error) {
	impl, err := s.ClientImpl.OpenTorrent(ctx, info, hash)
	if err != nil {
		return impl, err
	}
	piece := impl.Piece
	impl.Piece = func(p metainfo.Piece) storage.PieceImpl {
		return verificationBarrierPiece{PieceImpl: piece(p), owner: s, name: info.Name, index: p.Index()}
	}
	return impl, nil
}

type verificationBarrierPiece struct {
	storage.PieceImpl
	owner verificationBarrierStorage
	name  string
	index int
}

func (p verificationBarrierPiece) BeginVerification() error {
	r := verificationRequest{p.name, p.index, make(chan struct{})}
	select {
	case p.owner.requests <- r:
	case <-p.owner.stop:
		return context.Canceled
	}
	select {
	case <-r.proceed:
	case <-p.owner.stop:
		return context.Canceled
	}
	if impl, ok := p.PieceImpl.(interface{ BeginVerification() error }); ok {
		return impl.BeginVerification()
	}
	return nil
}
func (p verificationBarrierPiece) VerificationResources() ([]string, error) {
	return []string{"test-disk"}, nil
}

func schedulerFixture(t *testing.T, budget *HashBudget) (*Client, func(string) *Torrent, <-chan verificationRequest) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	backend := storage.NewFileOpts(storage.NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, NoCompletionRename: true, PieceCompletion: storage.NewMapPieceCompletion()})
	t.Cleanup(func() { backend.Close() })
	barrier := verificationBarrierStorage{backend, make(chan verificationRequest), make(chan struct{})}
	cfg := TestingConfig(t)
	cfg.DisableTCP, cfg.DisableUTP, cfg.DisableWebtorrent = true, true, true
	cfg.DefaultStorage, cfg.HashBudget = barrier, budget
	cfg.PieceHashersPerTorrent, cfg.MaxActivePieceHashers = 1, 1
	cl, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { close(barrier.stop); cl.Close() })
	add := func(name string) *Torrent {
		data := bytes.Repeat([]byte("good"), 64)
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		hash := sha1.Sum(data[:4])
		encoded, err := bencode.Marshal(metainfo.Info{Name: name, Length: int64(len(data)), PieceLength: 4, Pieces: bytes.Repeat(hash[:], 64)})
		if err != nil {
			t.Fatal(err)
		}
		spec := TorrentSpecFromMetaInfo(&metainfo.MetaInfo{InfoBytes: encoded})
		spec.DisableNetworking, spec.DisallowDataDownload = true, true
		tor, _, err := cl.AddTorrentSpec(spec)
		if err != nil {
			t.Fatal(err)
		}
		return tor
	}
	return cl, add, barrier.requests
}
func nextVerification(t *testing.T, requests <-chan verificationRequest) verificationRequest {
	t.Helper()
	select {
	case r := <-requests:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("no verification progress")
		return verificationRequest{}
	}
}

func TestVerificationReaderPassesBacklog(t *testing.T) {
	_, add, requests := schedulerFixture(t, NewHashBudget(2, 1, 64, 8))
	background := add("background")
	first := nextVerification(t, requests)
	interactive := add("interactive")
	interactive.EnableNetworking()
	interactive.AllowDataDownload()
	reader := interactive.NewReader()
	defer reader.Close()
	reader.SetReadahead(0)
	_, _ = reader.Seek(63*4, io.SeekStart)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	read := make(chan error, 1)
	go func() {
		out := make([]byte, 4)
		n, err := reader.ReadContext(ctx, out)
		if n == len(out) && err == io.EOF {
			err = nil
		}
		if err == nil && !bytes.Equal(out, []byte("good")) {
			err = io.ErrUnexpectedEOF
		}
		read <- err
	}()
	for interactive.FileObservations()[0].ReaderPieces == 0 {
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		runtime.Gosched()
	}
	select {
	case err := <-read:
		t.Fatalf("unverified read returned: %v", err)
	default:
	}
	close(first.proceed)
	next := nextVerification(t, requests)
	if next.name != "interactive" || next.piece != 63 {
		t.Fatalf("reader did not pass backlog: %+v", next)
	}
	close(next.proceed)
	select {
	case err := <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if background.DataDownloadAllowed() {
		t.Fatal("verification resumed background torrent")
	}
	if background.BytesCompleted() == background.Length() {
		t.Fatal("range waited for all background data")
	}
}

func TestVerificationRepeatedSeekDoesNotStarveBackground(t *testing.T) {
	cl, add, requests := schedulerFixture(t, nil)
	interactive := add("interactive")
	current := nextVerification(t, requests)
	background := add("background")
	interactive.SetVerificationSelection(nil)
	background.SetVerificationSelection(nil)
	seen := map[int]bool{}
	for turn := 0; turn < 28; turn++ {
		// Publish a new seek before each bounded piece completes. This isolates
		// scheduler fairness from OS/player scheduling of actual Reader calls.
		cl.lock()
		interactive._readerNowPieces.Clear()
		interactive._readerNowPieces.Add(bitmap.BitIndex(63 - turn))
		cl.unlock()
		close(current.proceed)
		current = nextVerification(t, requests)
		if current.name == "background" {
			seen[current.piece] = true
		}
	}
	if !seen[0] || !seen[1] {
		t.Fatalf("background did not advance through repeated seeks: %v", seen)
	}
	if interactive.DataDownloadAllowed() || background.DataDownloadAllowed() {
		t.Fatal("seek changed paused admission")
	}
}

func TestVerificationDropCancelsSharedBudgetWait(t *testing.T) {
	budget := NewHashBudget(2, 1, 64, 8)
	_, addA, requestsA := schedulerFixture(t, budget)
	_, addB, requestsB := schedulerFixture(t, budget)
	_ = addA("first")
	first := nextVerification(t, requestsA)
	second := addB("second")
	waitHashBudget(t, budget, 1)
	dropped := make(chan struct{})
	go func() { second.Drop(); close(dropped) }()
	select {
	case <-dropped:
	case <-time.After(3 * time.Second):
		t.Fatal("drop did not cancel queued check")
	}
	if s := budget.Stats(); s.Active != 1 || s.Pending != 0 || s.PeakActive != 1 {
		t.Fatal(s)
	}
	select {
	case <-requestsB:
		t.Fatal("cancelled check reached storage")
	default:
	}
	close(first.proceed)
}

func TestVerificationLayerUpdateAllowsUnrelatedTorrentWhileHashBlocked(t *testing.T) {
	cl, add, requests := schedulerFixture(t, NewHashBudget(2, 1, 64, 8))
	tor := add("blocked")
	first := nextVerification(t, requests)
	updating, done := make(chan struct{}), make(chan struct{})
	go func() { cl.lock(); close(updating); tor.piece(0).setV2Hash([32]byte{1}); cl.unlock(); close(done) }()
	<-updating
	available := make(chan struct{})
	go func() { _ = cl.Torrents(); close(available) }()
	select {
	case <-available:
	case <-time.After(time.Second):
		close(first.proceed)
		t.Fatal("piece layer update held Client.lock while waiting for storage")
	}
	close(first.proceed)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("piece layer update deadlocked with hasher completion")
	}
}
