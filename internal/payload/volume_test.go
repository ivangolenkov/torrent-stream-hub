package payload

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnrolledVolumeMissingMarkerNeverRepairsItself(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	v := VolumeRegistration{Root: ".", Token: strings.Repeat("a", 32), State: "enrolling"}
	if err := EnrollVolume(root, v); err != nil {
		t.Fatal(err)
	}
	v.State = "ready"
	if err := CheckVolume(root, filepath.Join("payload", "future"), []VolumeRegistration{v}); err != nil {
		t.Fatal(err)
	}
	if err := root.Remove(VolumeMarker); err != nil {
		t.Fatal(err)
	}
	if err := CheckVolume(root, ".", []VolumeRegistration{v}); !errors.Is(err, ErrVolumeUnavailable) {
		t.Fatal(err)
	}
	if err := EnrollVolume(root, v); !errors.Is(err, ErrVolumeUnavailable) {
		t.Fatal("ready enrollment recreated marker", err)
	}
	if _, err := root.Stat(VolumeMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("marker recreated", err)
	}
}
