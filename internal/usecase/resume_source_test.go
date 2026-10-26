package usecase

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"torrent-stream-hub/internal/fastresume"

	"torrent-stream-hub/internal/models"
)

func TestProductionResumeSourceSealsPausedAndRejectsStaleAttachment(t *testing.T) {
	uc, _, done := setupTorrentUseCase(t)
	defer done()
	tor, _ := moveFixture(t, uc, false, bytes.Repeat([]byte("good"), 8192))
	m, fixture := sealManagedResume(t, uc, tor.Hash)
	intent, _ := uc.repo.GetIntent(tor.Hash)
	if err := bindVerificationRuntime(uc.engine, uc.repo, intent.Version); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	release, err := uc.lockRecord(tor.Hash, ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	hash, mapping, source, err := uc.productionResumeSource(ctx, m.StorageID)
	if err != nil || hash != tor.Hash || mapping == nil {
		t.Fatal("production source missing", hash, mapping, err)
	}
	if err := uc.repo.SealResume(ctx, mapping, source); err != nil {
		t.Fatal("paused production source refused seal", err)
	}
	saved, err := uc.repo.LoadResume(ctx, m, fixture.Identity.RecordID)
	if err != nil || saved == nil || saved.Invalidated || saved.Token.Generation != 2 {
		t.Fatal(saved, err)
	}
	if err := uc.engine.Delete(tor.Hash); err != nil {
		t.Fatal(err)
	}
	if err := source.Authorize(ctx); err == nil {
		t.Fatal("detached source still authorized")
	}
}

func TestProductionResumeSourceRejectsDurableSafety(t *testing.T) {
	uc, repo, done := setupTorrentUseCase(t)
	defer done()
	tor, _ := moveFixture(t, uc, false, bytes.Repeat([]byte("good"), 8192))
	m, _ := sealManagedResume(t, uc, tor.Hash)
	ctx := context.Background()
	boundIntent, _ := repo.GetIntent(tor.Hash)
	if err := bindVerificationRuntime(uc.engine, repo, boundIntent.Version); err != nil {
		t.Fatal(err)
	}
	_, _, source, err := uc.productionResumeSource(ctx, m.StorageID)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Authorize(ctx); err != nil {
		t.Fatal(err)
	}
	intent, err := repo.GetIntent(tor.Hash)
	if err != nil {
		t.Fatal(err)
	}
	_, fence := uc.engine.RuntimeBinding(tor.Hash)
	if err := repo.PutActivationBlock(fence, models.BlockVerification, "test"); err != nil {
		t.Fatal(err)
	}
	if err := source.Authorize(ctx); err == nil {
		t.Fatal("verification block bypassed")
	}
	if err := repo.ClearActivationBlock(fence, models.BlockVerification); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveDownloadBlock(tor.Hash, "disk_full"); err != nil {
		t.Fatal(err)
	}
	if err := source.Authorize(ctx); err == nil {
		t.Fatal("disk safety bypassed", intent)
	}
}

func TestProductionResumeDirtyScanUsesCachedRevision(t *testing.T) {
	uc, _, done := setupTorrentUseCase(t)
	defer done()
	tor, _ := moveFixture(t, uc, false, bytes.Repeat([]byte("good"), 8192))
	m, _ := sealManagedResume(t, uc, tor.Hash)
	marked := 0
	mark := func(owner string) bool {
		if owner != m.StorageID {
			t.Error("unexpected owner", owner)
		}
		marked++
		return false // saturation must not acknowledge the dirty revision
	}
	if cursor, err := uc.scanResumeDirty(context.Background(), "", mark); err != nil || cursor != "" || marked != 0 {
		t.Fatal("clean owner queued", cursor, marked, err)
	}
	raw := uc.engine.GetRawTorrent(tor.Hash)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := raw.VerifyDataContext(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := uc.scanResumeDirty(ctx, "", mark); err != nil {
			t.Fatal(err)
		}
	}
	if marked != 2 {
		t.Fatal("dirty revision lost on saturation", marked)
	}
}

func TestProductionResumeSourceShutdownUsesIndependentBudget(t *testing.T) {
	uc, repo, done := setupTorrentUseCase(t, true)
	defer done()
	tor, _ := moveFixture(t, uc, false, bytes.Repeat([]byte("good"), 8192))
	m, fixture := sealManagedResume(t, uc, tor.Hash)
	intent, _ := repo.GetIntent(tor.Hash)
	if err := bindVerificationRuntime(uc.engine, repo, intent.Version); err != nil {
		t.Fatal(err)
	}
	p, err := newApplicationResumeProducer(uc, uc.productionResumeSource)
	if err != nil {
		t.Fatal(err)
	}
	if !uc.engine.RegisterStorageFinalizer(p) {
		_ = p.Shutdown(context.Background())
		_ = p.Wait()
		t.Fatal("finalizer admission failed")
	}
	if !p.MarkDirty(m.StorageID) {
		t.Fatal("queue admission failed")
	}
	uc.engine.Close()
	if err := p.Wait(); err != nil {
		t.Fatal("production shutdown failed", err)
	}
	saved, err := repo.LoadResume(context.Background(), m, fixture.Identity.RecordID)
	if err != nil || saved == nil || saved.Invalidated || saved.Token.Generation != 2 {
		t.Fatal("shutdown omitted real payload checkpoint", saved, err)
	}
}

func TestProductionResumeMetadataBudgetsBeforeDecode(t *testing.T) {
	uc, _, done := setupTorrentUseCase(t)
	defer done()
	info := &metainfo.Info{Name: "file", PieceLength: 4, Length: 4, Pieces: make([]byte, 20)}
	if err := os.MkdirAll(filepath.Join(uc.engine.ConfigDir(), "metainfo"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(uc.engine.ConfigDir(), "metainfo", testInfoHash+".torrent")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate(fastresume.MaxCheckpointBytes + 1)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uc.resumeOpenMetadata(testInfoHash, info); !errors.Is(err, fastresume.ErrBudget) {
		t.Fatal("oversize metadata admitted", err)
	}
	if err := os.WriteFile(path, []byte("d4:info99999999999999999:xe"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.resumeOpenMetadata(testInfoHash, info); err == nil {
		t.Fatal("unbounded declared string admitted")
	}
	info.Files = make([]metainfo.FileInfo, fastresume.MaxFiles+1)
	if _, err := resumeFileCount(info); !errors.Is(err, fastresume.ErrBudget) {
		t.Fatal("unbounded file catalog admitted", err)
	}
}
