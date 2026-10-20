package logging

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const dateLayout = "2006-01-02"

// FileOptions controls daily logging. RetentionDays counts previous calendar days.
type FileOptions struct {
	Dir             string
	RetentionDays   int
	Console         io.Writer
	Errors          io.Writer
	Now             func() time.Time
	CleanupInterval time.Duration
}

// Files owns the writer and retention worker. Readers hold independent descriptors;
// their names are pinned against retention until the snapshot is closed.
type Files struct {
	mu        sync.Mutex
	opts      FileOptions
	root      *os.Root
	file      io.WriteCloser
	active    string
	refs      map[string]int
	retryAt   time.Time
	reportAt  time.Time
	stop      chan struct{}
	done      chan struct{}
	closed    bool
	closeOnce sync.Once
}

func NewFiles(opts FileOptions) (*Files, error) {
	if opts.RetentionDays < 0 || opts.RetentionDays > 36500 {
		return nil, errors.New("log retention days must be between 0 and 36500")
	}
	if opts.Dir == "" {
		return nil, errors.New("log directory is required")
	}
	if opts.Console == nil {
		opts.Console = os.Stdout
	}
	if opts.Errors == nil {
		opts.Errors = os.Stderr
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.CleanupInterval <= 0 {
		opts.CleanupInterval = time.Hour
	}
	f := &Files{opts: opts, refs: make(map[string]int), stop: make(chan struct{}), done: make(chan struct{})}
	f.mu.Lock()
	f.cleanupLocked()
	f.mu.Unlock()
	go func() {
		defer close(f.done)
		ticker := time.NewTicker(opts.CleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				f.mu.Lock()
				if !f.closed {
					f.cleanupLocked()
				}
				f.mu.Unlock()
			case <-f.stop:
				return
			}
		}
	}()
	return f, nil
}

func (f *Files) reportLocked(err error) {
	now := f.opts.Now()
	if now.Before(f.reportAt) {
		return
	}
	f.reportAt = now.Add(time.Minute)
	_, _ = fmt.Fprintf(f.opts.Errors, "file logging unavailable: %s\n", SanitizeText(err.Error()))
}

func (f *Files) rootLocked() error {
	if f.root != nil {
		return nil
	}
	if err := os.MkdirAll(f.opts.Dir, 0700); err != nil {
		return err
	}
	root, err := os.OpenRoot(f.opts.Dir)
	if err != nil {
		return err
	}
	f.root = root
	return nil
}

func logDate(name string) (string, bool) {
	if len(name) != len("hub-2006-01-02.log") || !strings.HasPrefix(name, "hub-") || !strings.HasSuffix(name, ".log") {
		return "", false
	}
	date := name[4:14]
	t, err := time.Parse(dateLayout, date)
	return date, err == nil && t.Format(dateLayout) == date
}

func (f *Files) namesLocked() ([]string, error) {
	if err := f.rootLocked(); err != nil {
		return nil, err
	}
	dir, err := f.root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if _, ok := logDate(entry.Name()); !ok {
			continue
		}
		info, err := f.root.Lstat(entry.Name())
		if err != nil {
			return nil, err
		}
		if info.Mode().IsRegular() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func (f *Files) cleanupLocked() {
	names, err := f.namesLocked()
	if err != nil {
		f.reportLocked(err)
		return
	}
	cutoff := f.opts.Now().AddDate(0, 0, -f.opts.RetentionDays).Format(dateLayout)
	for _, name := range names {
		date, _ := logDate(name)
		if date >= cutoff || name == f.active || f.refs[name] > 0 {
			continue
		}
		if err := f.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			f.reportLocked(err)
		}
	}
}

func (f *Files) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	originalLen := len(p)
	// Sanitize every source, including standard log and library adapters.
	p = []byte(SanitizeText(string(p)))
	_, consoleErr := f.opts.Console.Write(p)
	if f.closed {
		return originalLen, consoleErr
	}
	now := f.opts.Now()
	name := "hub-" + now.Format(dateLayout) + ".log"
	if f.active != name && f.file != nil {
		if err := f.file.Close(); err != nil {
			f.reportLocked(err)
		}
		f.file, f.active = nil, ""
		f.cleanupLocked()
	}
	if f.file == nil && !now.Before(f.retryAt) {
		err := f.rootLocked()
		if err == nil {
			info, statErr := f.root.Lstat(name)
			if statErr == nil && !info.Mode().IsRegular() {
				err = errors.New("log target is not a regular file")
			} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				err = statErr
			}
		}
		if err == nil {
			var file *os.File
			file, err = f.root.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err == nil {
				f.file = file
			}
		}
		if err != nil {
			f.retryAt = now.Add(time.Minute)
			f.reportLocked(err)
		} else {
			f.active = name
		}
	}
	if f.file != nil {
		if n, err := f.file.Write(p); err != nil || n != len(p) {
			if err == nil {
				err = io.ErrShortWrite
			}
			f.reportLocked(err)
			_ = f.file.Close()
			f.file, f.active = nil, ""
			f.retryAt = now.Add(time.Minute)
		}
	}
	// A file failure must not disable console output or fail the caller.
	return originalLen, consoleErr
}

func (f *Files) Close() error {
	f.closeOnce.Do(func() {
		close(f.stop)
		<-f.done
		f.mu.Lock()
		defer f.mu.Unlock()
		f.closed = true
		if f.file != nil {
			if err := f.file.Close(); err != nil {
				f.reportLocked(err)
			}
			f.file = nil
		}
		if f.root != nil {
			_ = f.root.Close()
			f.root = nil
		}
	})
	return nil
}
