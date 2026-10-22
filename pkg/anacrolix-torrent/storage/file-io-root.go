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
	if flags&os.O_CREATE != 0 {
		if err := r.root.MkdirAll(filepath.Dir(rel), dirPerm); err != nil {
			return nil, err
		}
	}
	f, err := r.root.OpenFile(rel, flags, filePerm)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || FileHasMultipleLinks(f, st) {
		f.Close()
		return nil, fmt.Errorf("unsafe storage file")
	}
	return f, nil
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
	f, err := r.open(name, os.O_RDONLY)
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
	if _, err := r.root.Lstat(a); err != nil {
		return err
	}
	if _, err := r.root.Lstat(b); err == nil {
		return fmt.Errorf("storage rename target exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Link is atomic no-replace, unlike Rename on Unix. Both names are in the
	// same directory/filesystem; never overwrite a concurrently created target.
	if err := r.root.Link(a, b); err != nil {
		return err
	}
	return r.root.Remove(a)
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
