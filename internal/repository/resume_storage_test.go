package repository

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/merkle"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
	"torrent-stream-hub/internal/fastresume"
)

// Real payload + SQLite close/reopen: conversion, durable revoke and the final
// epoch check must agree with the backend's one-open token. This is deliberately
// not the production writable adapter (mutation hooks are not installed yet).
func TestResumeStorageRoundTrip(t *testing.T) {
	for _, version := range []string{"v1", "v2", "hybrid"} {
		t.Run(version, func(t *testing.T) {
			ctx := context.Background()
			db, repo, mapping, initial, epoch := resumeFixture(t)
			data := bytes.Repeat([]byte("good"), 8192)
			info := metainfo.Info{Name: "Test", PieceLength: 16384, Files: []metainfo.FileInfo{{Path: []string{"video.bin"}, Length: int64(len(data))}}}
			mi := &metainfo.MetaInfo{}
			if version != "v2" {
				for off := 0; off < len(data); off += 16384 {
					h := sha1.Sum(data[off : off+16384])
					info.Pieces = append(info.Pieces, h[:]...)
				}
			}
			if version != "v1" {
				info.MetaVersion = 2
				if version == "v2" {
					info.Files = nil
				}
				h := merkle.NewHash()
				h.Write(data)
				root := string(h.Sum(nil))
				info.FileTree = metainfo.FileTree{Dir: map[string]metainfo.FileTree{"video.bin": {File: metainfo.FileTreeFile{Length: int64(len(data)), PiecesRoot: root}}}}
				var layer []byte
				for off := 0; off < len(data); off += 16384 {
					h := merkle.NewHash()
					h.Write(data[off : off+16384])
					layer = h.SumMinLength(layer, 16384)
				}
				mi.PieceLayers = map[string]string{root: string(layer)}
			}
			var err error
			mi.InfoBytes, err = bencode.Marshal(&info)
			if err != nil {
				t.Fatal(err)
			}
			hash := mi.HashInfoBytes()
			if version == "v2" {
				digest := sha256.Sum256(mi.InfoBytes)
				copy(hash[:], digest[:])
			}
			dir := t.TempDir()
			path := filepath.Join(dir, mapping.Root, "Test", "video.bin")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			var reads atomic.Int64
			open := func(readOnly bool) storage.TorrentImpl {
				root, err := os.OpenRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				backend := storage.NewFileOpts(storage.NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, ReadOnly: readOnly, NoCompletionRename: true, UsePartFiles: g.Some(false), PieceCompletion: storage.NewMapPieceCompletion(),
					IORead: func(_ string, n int64) { reads.Add(n) },
					TorrentPathResolver: func(base string, _ *metainfo.Info, _ metainfo.Hash) (string, []string, error) {
						return filepath.Join(base, mapping.Root), []string{filepath.Join("Test", "video.bin")}, nil
					},
				})
				t.Cleanup(func() { _ = backend.Close() })
				ts, err := backend.OpenTorrent(ctx, &info, hash)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = ts.Close() })
				return ts
			}
			ts := open(false)
			piece := func(ts storage.TorrentImpl, i int) storage.PieceImpl {
				if ts.PieceWithHash != nil {
					return ts.PieceWithHash(info.Piece(i), g.None[[]byte]())
				}
				return ts.Piece(info.Piece(i))
			}
			for i := 0; i < 2; i++ {
				p := piece(ts, i)
				if err := p.(interface{ BeginVerification() error }).BeginVerification(); err != nil {
					t.Fatal(err)
				}
				buf := make([]byte, 16384)
				if n, err := p.ReadAt(buf, 0); err != nil || n != len(buf) || !bytes.Equal(buf, data[i*16384:(i+1)*16384]) {
					t.Fatal(n, err)
				}
				if err := p.MarkComplete(); err != nil {
					t.Fatal(err)
				}
			}
			source := ResumeSource{Identity: initial.Identity, Metadata: mi, Info: &info,
				Authorize: func(ctx context.Context) error { return ctx.Err() },
				Capture: func(ctx context.Context, publish func(storage.FileCheckpoint) error) error {
					return ts.WithCheckpoint(ctx, []byte{3}, publish)
				},
			}
			worker, err := repo.NewResumeProducer(func(context.Context, string) (*StorageMapping, ResumeSource, func(), error) {
				return mapping, source, func() {}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = worker.Shutdown(context.Background()); _ = worker.Wait() })
			if !worker.MarkDirty(mapping.StorageID) {
				t.Fatal("checkpoint request rejected")
			}
			if err := worker.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			if err := worker.Wait(); err != nil {
				t.Fatal(err)
			}
			sealed, err := repo.LoadResume(ctx, mapping, epoch.RecordID)
			if err != nil {
				t.Fatal(err)
			}
			epoch = sealed.Token
			// Failed publication cannot replace the committed generation.
			if err := ts.WithCheckpoint(ctx, []byte{3}, func(native storage.FileCheckpoint) error {
				c, err := fastresume.FromStorage(initial.Identity, mi, &info, 2, time.Now().UnixMilli(), native)
				if err != nil {
					return err
				}
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				_, err = repo.CommitResume(cancelled, mapping, epoch, c)
				return err
			}); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			epoch, err = repo.RevokeResume(ctx, mapping, epoch, []byte{2})
			if err != nil {
				t.Fatal(err)
			}
			if err := ts.Close(); err != nil {
				t.Fatal(err)
			}
			var dbPath string
			if err := db.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&dbPath); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := NewSQLiteDB(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			repo = NewTorrentRepo(reopened)
			snapshot, err := repo.LoadResume(ctx, mapping, epoch.RecordID)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Token.Generation != 1 {
				t.Fatal("failed commit replaced checkpoint")
			}
			claims, err := fastresume.StorageClaims(initial.Identity, mi, &info, snapshot.Checkpoint, snapshot.Revoked)
			if err != nil {
				t.Fatal(err)
			}
			ro := open(true)
			reads.Store(0)
			check := func(ctx context.Context) error {
				s, err := repo.LoadResume(ctx, mapping, epoch.RecordID)
				if err != nil {
					return err
				}
				if s.Token != snapshot.Token || s.Invalidated {
					return ErrStateConflict
				}
				return nil
			}
			if err := repo.RestoreResume(ctx, mapping, source, ro); err != nil {
				t.Fatal(err)
			}
			if reads.Load() != 0 || !ro.CachedComplete(0) || ro.CachedComplete(1) {
				t.Fatal("wrong trust subset or payload hashing")
			}
			buf := make([]byte, 16)
			if n, err := piece(ro, 0).ReadAt(buf, 0); (err != nil && err != io.EOF) || n != 16 || !bytes.Equal(buf, data[:16]) {
				t.Fatal(n, err)
			}
			late := open(true)
			token, err := late.PrepareRestore(ctx, claims, check)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repo.InvalidateResume(ctx, mapping, snapshot.Token); err != nil {
				t.Fatal(err)
			}
			if err := late.RestoreCheckpoint(ctx, token); !errors.Is(err, ErrStateConflict) {
				t.Fatal("late token bypassed durable invalidation", err)
			}
			if late.CachedComplete(0) {
				t.Fatal("failed restore attached trust")
			}
		})
	}
}
