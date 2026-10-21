package payload

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRootContainedPayloadOpen(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.WriteFile(filepath.Join(dir, "valid"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := Open(root, "valid")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := os.Mkdir(filepath.Join(dir, "folder"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".", "..", "../outside", filepath.Join(dir, "valid"), "folder"} {
		if f, err := Open(root, name); err == nil {
			f.Close()
			t.Fatalf("unsafe name accepted: %s", name)
		}
	}
	if err := os.Symlink("valid", filepath.Join(dir, "alias")); err != nil {
		t.Skip(err)
	}
	if f, err := Open(root, "alias"); err == nil {
		f.Close()
		t.Fatal("even an internal symlink must not be treated as owned payload")
	}
}
