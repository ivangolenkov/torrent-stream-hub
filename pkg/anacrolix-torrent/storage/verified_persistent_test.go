package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type persistentTestCompletion struct{ PieceCompletion }

func (persistentTestCompletion) Persistent() bool { return true }

func TestVerifiedPersistentCompletionFlushesExistingFile(t *testing.T) {
	for _, name := range []string{"data", "data.part"} {
		t.Run(name, func(t *testing.T) {
			dir, backend, info, ts := verifiedFixture(t)
			backend.opts.PieceCompletion = persistentTestCompletion{NewMapPieceCompletion()}
			if err := os.WriteFile(filepath.Join(dir, name), []byte("good"), 0600); err != nil {
				t.Fatal(err)
			}
			p := ts.Piece(info.Piece(0)).(*filePieceImpl)
			if err := p.BeginVerification(); err != nil {
				t.Fatal(err)
			}
			if err := p.MarkComplete(); err != nil {
				t.Fatalf("verified existing file rejected by persistent completion: %v", err)
			}
			if !p.Completion().Complete || !ts.FreshComplete(0) {
				t.Fatal("freshly verified data lost completion")
			}
			if data, err := os.ReadFile(filepath.Join(dir, "data")); err != nil || string(data) != "good" {
				t.Fatalf("published file changed: %q %v", data, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "data.part")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unexpected part file after verification: %v", err)
			}
		})
	}
}

func TestVerifiedPersistentCompletionPreservesFlushError(t *testing.T) {
	dir, backend, info, ts := verifiedFixture(t)
	backend.opts.PieceCompletion = persistentTestCompletion{NewMapPieceCompletion()}
	if err := os.WriteFile(filepath.Join(dir, "data.part"), []byte("good"), 0600); err != nil {
		t.Fatal(err)
	}
	p := ts.Piece(info.Piece(0)).(*filePieceImpl)
	if err := p.BeginVerification(); err != nil {
		t.Fatal(err)
	}
	p.t.io = failPublicationFlush{p.t.io}
	if err := p.MarkComplete(); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("flush failure hidden: %v", err)
	}
	if p.Completion().Complete || ts.FreshComplete(0) {
		t.Fatal("failed flush published completion")
	}
}
