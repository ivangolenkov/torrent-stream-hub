package storage

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

func TestVerificationResourcesUseFilesystemNotDirectory(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	backend := NewFileOpts(NewFileClientOpts{ClientBaseDir: dir, RootFS: root, VerifyOnOpen: true, NoCompletionRename: true, PieceCompletion: NewMapPieceCompletion()})
	defer backend.Close()
	info := &metainfo.Info{Name: "library", PieceLength: 4, Pieces: make([]byte, 40), Files: []metainfo.FileInfo{
		{Length: 3, Path: []string{"existing", "one"}}, {Length: 3, Path: []string{"not-created", "two"}},
	}}
	if err := os.MkdirAll(filepath.Join(dir, "library", "existing"), 0700); err != nil {
		t.Fatal(err)
	}
	impl, err := backend.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer impl.Close()
	keys := func(index int) []string {
		t.Helper()
		keys, err := impl.Piece(info.Piece(index)).(interface{ VerificationResources() ([]string, error) }).VerificationResources()
		if err != nil {
			t.Fatal(err)
		}
		return keys
	}
	first, second := keys(0), keys(1)
	if len(first) != 1 || !reflect.DeepEqual(first, second) {
		t.Fatalf("same filesystem treated as independent disks: %v %v", first, second)
	}
	if _, err := os.Stat(filepath.Join(dir, "library", "not-created")); !os.IsNotExist(err) {
		t.Fatal("resource resolution created payload directories", err)
	}
}
