package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// rootFileIo never falls back to absolute filesystem operations.
type rootFileIo struct {
	root *os.Root
	ctx  context.Context
}

var ErrUnsafeFile = errors.New("unsafe storage file")

// OpenRootFile is the shared payload read/write boundary. Names are root-relative,
// symlinks and hardlinks are rejected, and existing file identity is revalidated
// after the rooted open. Callers own the returned descriptor, never the root.
func OpenRootFile(root *os.Root, name string, flags int) (*os.File, error) {
	if !filepath.IsLocal(name) || filepath.Clean(name) == "." {
		return nil, ErrUnsafeFile
	}
	r := rootFileIo{root: root}
	rel, err := r.relative(filepath.Join(root.Name(), name))
	if err != nil {
		return nil, err
	}
	expected, err := root.Lstat(rel)
	if err != nil && !(flags&os.O_CREATE != 0 && errors.Is(err, os.ErrNotExist)) {
		return nil, err
	}
	if flags&os.O_CREATE != 0 {
		if err := root.MkdirAll(filepath.Dir(rel), dirPerm); err != nil {
			return nil, err
		}
	}
	f, err := root.OpenFile(rel, flags, filePerm)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || FileHasMultipleLinks(f, st) || expected != nil && !os.SameFile(expected, st) {
		f.Close()
		return nil, ErrUnsafeFile
	}
	return f, nil
}

func (r rootFileIo) relative(name string) (string, error) {
	base, err := filepath.Abs(r.root.Name())
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(base, abs)
	if err != nil || !filepath.IsLocal(rel) || rel == "." {
		return "", fmt.Errorf("storage path outside root")
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	for i := range parts {
		st, err := r.root.Lstat(filepath.Join(parts[:i+1]...))
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return "", err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("storage symlink rejected")
		}
		if i == len(parts)-1 && !st.Mode().IsRegular() {
			return "", fmt.Errorf("non-regular storage entry")
		}
	}
	return rel, nil
}

func (r rootFileIo) open(name string, flags int) (*os.File, error) {
	if r.ctx != nil && r.ctx.Err() != nil {
		return nil, r.ctx.Err()
	}
	rel, err := r.relative(name)
	if err != nil {
		return nil, err
	}
	return OpenRootFile(r.root, rel, flags)
}

type rootSharedReader struct {
	*os.File
	ctx context.Context
}

func (r rootSharedReader) ReadAt(p []byte, off int64) (int, error) {
	if r.ctx != nil && r.ctx.Err() != nil {
		return 0, r.ctx.Err()
	}
	return r.File.ReadAt(p, off)
}

type rootReader struct {
	classicFileReader
	ctx context.Context
}

func (r rootReader) Read(p []byte) (int, error) {
	if r.ctx != nil && r.ctx.Err() != nil {
		return 0, r.ctx.Err()
	}
	return r.File.Read(p)
}
func (r rootReader) writeToN(w io.Writer, n int64) (int64, error) {
	return io.CopyBuffer(w, io.LimitReader(struct{ io.Reader }{r}, n), make([]byte, 128<<10))
}
func (r rootFileIo) openForSharedRead(name string) (sharedFileIf, error) {
	f, err := r.open(name, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	return rootSharedReader{f, r.ctx}, nil
}
func (r rootFileIo) openForRead(name string) (fileReader, error) {
	f, err := r.open(name, os.O_RDONLY)
	return rootReader{classicFileReader{f}, r.ctx}, err
}
func (r rootFileIo) openForWrite(name string, _ int64) (fileWriter, error) {
	return r.open(name, os.O_WRONLY|os.O_CREATE)
}
func (r rootFileIo) flush(name string, _, _ int64) error {
	flags := os.O_RDONLY
	if runtime.GOOS == "windows" {
		flags = os.O_RDWR
	} // FlushFileBuffers requires write access.
	f, err := r.open(name, flags)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (r rootFileIo) rename(from, to string) error {
	a, err := r.relative(from)
	if err != nil {
		return err
	}
	b, err := r.relative(to)
	if err != nil {
		return err
	}
	source, err := r.open(from, os.O_RDONLY)
	if err != nil {
		return err
	}
	if err := source.Close(); err != nil {
		return err
	}
	if _, err := r.root.Lstat(b); err == nil {
		return fmt.Errorf("storage rename target exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if filepath.Dir(a) != filepath.Dir(b) {
		return fmt.Errorf("storage publication must stay in one directory")
	}
	dir, err := r.root.Open(filepath.Dir(a))
	if err != nil {
		return err
	}
	defer dir.Close()
	// One native operation publishes the file. Never emulate no-replace by
	// Link+Remove: interruption leaves hardlinks rejected by the read boundary.
	if err := renameNoReplace(dir, filepath.Base(a), filepath.Base(b)); err != nil {
		return &os.LinkError{Op: "publish", Old: a, New: b, Err: err}
	}
	return syncPublicationDirectory(dir)
}
func (r rootFileIo) stat(name string) (os.FileInfo, error) {
	if runtime.GOOS != "windows" {
		if r.ctx != nil && r.ctx.Err() != nil {
			return nil, r.ctx.Err()
		}
		rel, err := r.relative(name)
		if err != nil {
			return nil, err
		}
		st, err := r.root.Stat(rel)
		if err != nil {
			return nil, err
		}
		if !st.Mode().IsRegular() || FileHasMultipleLinks(nil, st) {
			return nil, fmt.Errorf("unsafe storage file")
		}
		return st, nil
	}
	f, err := r.open(name, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}
