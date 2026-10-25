package payload

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/anacrolix/torrent/storage"
)

var ErrVolumeUnavailable = errors.New("expected storage volume unavailable")
var ErrVolumeUnregistered = errors.New("storage volume requires explicit registration")

type VolumeRegistration struct{ Root, Token, State string }

const VolumeMarker = ".tsh-volume"

func volumeDirectory(root *os.Root, path string) (*os.File, error) {
	if path == "." {
		return root.Open(".")
	}
	return storage.OpenRootDirectory(root, path)
}

// FindVolumeRoot finds a device boundary within the managed root. Device IDs
// are compared during this call only. The persisted identity is the marker.
func FindVolumeRoot(root *os.Root, path string) (string, error) {
	if path != "." && (ValidateRelative(path) != nil || filepath.Clean(path) != path) {
		return "", ErrUnsafePath
	}
	if path != "." {
		if st, err := root.Lstat(path); err == nil && st.Mode().IsRegular() {
			path = filepath.Dir(path)
		}
	}
	var current string
	for {
		f, err := volumeDirectory(root, path)
		if errors.Is(err, os.ErrNotExist) {
			path = filepath.Dir(path)
			continue
		}
		if err != nil {
			return "", err
		}
		current, err = storage.FilesystemIdentity(f)
		f.Close()
		if err != nil {
			return "", err
		}
		break
	}
	for depth := 0; path != "."; depth++ {
		if depth >= 128 {
			return "", ErrUnsafePath
		}
		parent := filepath.Dir(path)
		f, err := volumeDirectory(root, parent)
		if err != nil {
			return "", err
		}
		device, err := storage.FilesystemIdentity(f)
		f.Close()
		if err != nil {
			return "", err
		}
		if device != current {
			return path, nil
		}
		path = parent
	}
	return path, nil
}

func CheckVolume(root *os.Root, path string, volumes []VolumeRegistration) error {
	best := -1
	for i, v := range volumes {
		if (v.Root == "." || path == v.Root || (Overlap(path, v.Root) && len(path) > len(v.Root))) && (best < 0 || len(v.Root) > len(volumes[best].Root)) {
			best = i
		}
	}
	if best < 0 || volumes[best].State != "ready" {
		return ErrVolumeUnregistered
	}
	v := volumes[best]
	f, err := Open(root, filepath.Join(v.Root, VolumeMarker))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrVolumeUnavailable, err)
	}
	data, err := io.ReadAll(io.LimitReader(f, 256))
	f.Close()
	if err != nil || string(data) != v.Token+"\n" {
		return ErrVolumeUnavailable
	}
	actual, err := FindVolumeRoot(root, path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrVolumeUnavailable, err)
	}
	if actual != v.Root {
		return ErrVolumeUnregistered
	}
	return nil
}

// Only the enrollment owner calls this after persisting its token. Ready
// registrations never recreate a missing marker. O_EXCL rejects foreign data.
func EnrollVolume(root *os.Root, v VolumeRegistration) error {
	if v.State != "enrolling" || len(v.Token) != 32 {
		return ErrVolumeUnavailable
	}
	marker := filepath.Join(v.Root, VolumeMarker)
	f, err := storage.OpenRootFile(root, marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if errors.Is(err, os.ErrExist) {
		v.State = "ready"
		return CheckVolume(root, v.Root, []VolumeRegistration{v})
	}
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(f, v.Token+"\n")
	err = errors.Join(writeErr, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	return storage.SyncRootDirectory(root, v.Root)
}
