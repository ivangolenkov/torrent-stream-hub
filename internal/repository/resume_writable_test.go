package repository

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent/storage"
)

func TestResumeMutationWritableLibrary(t *testing.T) {
	for _, version := range []string{"v1", "v2", "hybrid"} {
		t.Run(version, func(t *testing.T) {
			dir := t.TempDir()
			t.Run("initial-seal", func(t *testing.T) {
				r, m, source, _, _ := openResumeLibrary(t, dir, version, false)
				if err := r.SealResume(context.Background(), m, source); err != nil {
					t.Fatal(err)
				}
			})
			var native storage.TorrentImpl
			r, m, source, tor, reads := openResumeLibrary(t, dir, version, true, &native)
			assertResumeLibraryRange(t, tor, reads, false)
			path := filepath.Join(dir, m.Root, "video")
			if _, err := r.db.DB().Exec(`CREATE TRIGGER fail_mutation_barrier BEFORE UPDATE OF mutation_epoch ON resume_owners BEGIN SELECT RAISE(ABORT,'injected revoke failure'); END`); err != nil {
				t.Fatal(err)
			}
			piece := native.Piece(source.Info.Piece(0))
			if n, err := piece.WriteAt([]byte("evil"), 0); n != 0 || err == nil {
				t.Fatal("failed durable revoke admitted write", n, err)
			}
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, bytes.Repeat([]byte("good"), 8192)) {
				t.Fatal("payload changed before revoke commit", err)
			}
			if _, err := r.db.DB().Exec(`DROP TRIGGER fail_mutation_barrier`); err != nil {
				t.Fatal(err)
			}
			if _, err := piece.WriteAt([]byte("evil"), 0); err != nil {
				t.Fatal(err)
			}
			saved, err := r.LoadResume(context.Background(), m, source.Identity.RecordID)
			if err != nil || !bytes.Equal(saved.Revoked, []byte{1}) {
				t.Fatal("write was not durably revoked", saved, err)
			}
			if native.CachedComplete(0) || !native.CachedComplete(1) {
				t.Fatal("live revoke affected wrong pieces")
			}
			if _, err := piece.ReadAt(make([]byte, 4), 0); !errors.Is(err, storage.ErrUnverifiedPayload) {
				t.Fatal("unverified overwrite became readable", err)
			}
			if err := r.SealResume(context.Background(), m, source); err != nil {
				t.Fatal(err)
			}
			saved, err = r.LoadResume(context.Background(), m, source.Identity.RecordID)
			if err != nil || saved.Token.Generation != 2 || !bytes.Equal(saved.Checkpoint.Pieces, []byte{2}) {
				t.Fatal("seal admitted unverified overwrite", saved, err)
			}
			before := saved.Token
			if _, err := piece.WriteAt([]byte("more"), 0); err != nil {
				t.Fatal(err)
			}
			saved, err = r.LoadResume(context.Background(), m, source.Identity.RecordID)
			if err != nil || saved.Token != before {
				t.Fatal("untrusted chunk caused a safety transaction", saved, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := tor.VerifyDataContext(ctx); err != nil {
				t.Fatal(err)
			}
			saved, err = r.LoadResume(context.Background(), m, source.Identity.RecordID)
			if err != nil || !bytes.Equal(saved.Revoked, []byte{2}) || native.CachedComplete(0) {
				t.Fatal("explicit verify revived rejected trust", saved, err)
			}
		})
	}
}

func TestResumeMutationWritableEvidenceRevokeReload(t *testing.T) {
	for _, version := range []string{"v1", "v2", "hybrid"} {
		t.Run(version, func(t *testing.T) {
			dir := t.TempDir()
			t.Run("seal", func(t *testing.T) {
				r, m, source, _, _ := openResumeLibrary(t, dir, version, false)
				if err := r.SealResume(context.Background(), m, source); err != nil {
					t.Fatal(err)
				}
			})
			path := filepath.Join(dir, "payload", "film", "video")
			if err := os.Chtimes(path, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
				t.Fatal(err)
			}
			var native storage.TorrentImpl
			r, m, source, tor, reads := openResumeLibrary(t, dir, version, true, &native)
			assertResumeLibraryMask(t, tor, reads, 0)
			saved, err := r.LoadResume(context.Background(), m, source.Identity.RecordID)
			if err != nil || saved.Invalidated || !bytes.Equal(saved.Revoked, []byte{3}) {
				t.Fatal("observed evidence mismatch was not durably revoked", saved, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := tor.VerifyDataContext(ctx); err != nil {
				t.Fatal(err)
			}
			if !native.CachedComplete(0) || !native.CachedComplete(1) {
				t.Fatal("fresh verification could not recover revoked pieces")
			}
		})
	}
}
