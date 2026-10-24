package storage

import (
	"context"
	"errors"
	"github.com/anacrolix/torrent/metainfo"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRootPublicationAtomicNoReplace(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("no native publication primitive on this OS")
	}
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	r := rootFileIo{root: root, ctx: context.Background()}
	part, final := filepath.Join(dir, "movie.part"), filepath.Join(dir, "movie")
	if err := os.WriteFile(part, []byte("verified"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(final, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.rename(part, final); err == nil {
		t.Fatal("overwrote target")
	}
	if b, err := os.ReadFile(final); err != nil || string(b) != "foreign" {
		t.Fatal(string(b), err)
	}
	if err := os.Remove(final); err != nil {
		t.Fatal(err)
	}
	if err := r.rename(part, final); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatal("publication retained source", err)
	}
	f, err := r.open(final, os.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	// Reopening after publication never encounters a second hardlink name.
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	root, err = os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	r.root = root
	f, err = r.open(final, os.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestLegacyInterruptedPublicationPreservesUnprovenLinks(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	part, final := filepath.Join(dir, "movie.part"), filepath.Join(dir, "movie")
	if err := os.WriteFile(part, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(part, final); err != nil {
		t.Skipf("filesystem does not support hardlinks: %v", err)
	}
	r := rootFileIo{root: root, ctx: context.Background()}
	if err := r.rename(part, final); err == nil {
		t.Fatal("recovered without ownership evidence")
	}
	for _, p := range []string{part, final} {
		if b, err := os.ReadFile(p); err != nil || string(b) != "payload" {
			t.Fatal(p, err)
		}
	}
}

type failPublicationFlush struct{ fileIo }

func (failPublicationFlush) flush(string, int64, int64) error { return os.ErrPermission }

func TestPublicationFlushFailurePreservesPart(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	backend := NewFileOpts(NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true})
	defer backend.Close()
	info := &metainfo.Info{Name: "data", Length: 4, PieceLength: 4, Pieces: make([]byte, 20)}
	ts, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	p := ts.Piece(info.Piece(0)).(*filePieceImpl)
	if _, err := p.WriteAt([]byte("data"), 0); err != nil {
		t.Fatal(err)
	}
	p.t.io = failPublicationFlush{p.t.io}
	if err := p.BeginVerification(); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkComplete(); !errors.Is(err, os.ErrPermission) {
		t.Fatal("flush failure ignored", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "data")); !os.IsNotExist(err) {
		t.Fatal("published after failed flush", err)
	}
	if content, err := os.ReadFile(filepath.Join(dir, "data.part")); err != nil || string(content) != "data" {
		t.Fatal("part lost", string(content), err)
	}
}

type failPublicationAck struct{ fileIo }

func (f failPublicationAck) rename(from, to string) error {
	if err := f.fileIo.rename(from, to); err != nil {
		return err
	}
	return os.ErrPermission // Lost acknowledgement after the atomic namespace change.
}

func TestPublicationLostAcknowledgementReopensSafely(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	opts := NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true}
	backend := NewFileOpts(opts)
	info := &metainfo.Info{Name: "data", Length: 4, PieceLength: 4, Pieces: make([]byte, 20)}
	ts, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	p := ts.Piece(info.Piece(0)).(*filePieceImpl)
	if _, err := p.WriteAt([]byte("data"), 0); err != nil {
		t.Fatal(err)
	}
	p.t.io = failPublicationAck{p.t.io}
	if err := p.BeginVerification(); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkComplete(); !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
	ts.Close()
	backend.Close()
	if content, err := os.ReadFile(filepath.Join(dir, "data")); err != nil || string(content) != "data" {
		t.Fatal(string(content), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "data.part")); !os.IsNotExist(err) {
		t.Fatal("two names after publication", err)
	}
	root, err = os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	opts.RootFS = root
	backend = NewFileOpts(opts)
	defer backend.Close()
	ts, err = backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	if ts.FreshComplete(0) {
		t.Fatal("reopen trusted final filename")
	}
	p = ts.Piece(info.Piece(0)).(*filePieceImpl)
	if err := p.BeginVerification(); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkComplete(); err != nil {
		t.Fatal("cannot verify already published file", err)
	}
	if !ts.FreshComplete(0) {
		t.Fatal("verification did not restore readiness")
	}
}

func TestNativePublicationCannotReplaceConcurrentTarget(t *testing.T) {
	dir := t.TempDir()
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	for _, name := range []string{"first.part", "second.part"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, name := range []string{"first.part", "second.part"} {
		go func() { <-start; results <- renameNoReplace(parent, name, "x") }()
	}
	close(start)
	successes := 0
	for i := 0; i < 2; i++ {
		if <-results == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatal("native rename must have exactly one winner", successes)
	}
	content, err := os.ReadFile(filepath.Join(dir, "x"))
	if err != nil {
		t.Fatal(err)
	}
	winner := string(content)
	if winner != "first.part" && winner != "second.part" {
		t.Fatal(winner)
	}
	loser := "first.part"
	if winner == loser {
		loser = "second.part"
	}
	if content, err := os.ReadFile(filepath.Join(dir, loser)); err != nil || string(content) != loser {
		t.Fatal("losing payload lost", string(content), err)
	}
}
