package storage

import (
	"errors"
	"os"
	"path/filepath"
)

// Resolve live filesystem identity outside the client lock. A v1 boundary
// piece may span more than one mount; its hash lease covers every contributor.
func (p *filePieceImpl) VerificationResources() ([]string, error) {
	root := p.t.client.opts.RootFS
	if root == nil {
		return []string{"unrooted"}, nil
	}
	keys := make(map[string]bool)
	parents := make(map[string]bool)
	observe := func(op, name string) {
		if f := p.t.client.opts.IOEvent; f != nil {
			f("resource-"+op, name)
		}
	}
	for index := range p.t.segmentLocater.LocateIter(p.extent()) {
		path := filepath.Dir(p.t.files[index].safeOsPath)
		if parents[path] {
			continue
		}
		parents[path] = true
		rel, err := filepath.Rel(root.Name(), path)
		if err != nil || !filepath.IsLocal(rel) {
			return nil, ErrUnsafeFile
		}
		for {
			f, err := openRootDirectoryObserved(root, rel, observe)
			if errors.Is(err, os.ErrNotExist) && rel != "." {
				rel = filepath.Dir(rel)
				continue
			}
			if err != nil {
				return nil, err
			}
			observe("fstat", rel)
			key, err := FilesystemIdentity(f)
			f.Close()
			if err != nil {
				return nil, err
			}
			keys[key] = true
			break
		}
	}
	result := make([]string, 0, len(keys))
	for key := range keys {
		result = append(result, "fs/"+key)
	}
	return result, nil
}
