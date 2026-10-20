package logging

import (
	"context"
	"errors"
	"io"
	"os"
	"sort"
	"sync"
)

// Selection uses validated inclusive ISO calendar dates. Tail=0 means full output.
type Selection struct {
	From, To string
	Tail     int
}

type LogReader interface {
	OpenLogs(context.Context, Selection) (io.ReadCloser, error)
}

type logPart struct {
	file        *os.File // nil represents an inserted newline between daily files
	name        string
	start, size int64
}

type logSnapshot struct {
	owner        *Files
	ctx          context.Context
	parts        []logPart
	size, offset int64
	once         sync.Once
}

func (f *Files) OpenLogs(ctx context.Context, selection Selection) (io.ReadCloser, error) {
	if selection.From > selection.To || selection.Tail < 0 {
		return nil, errors.New("invalid log selection")
	}
	s := &logSnapshot{owner: f, ctx: ctx}
	err := func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.closed {
			return os.ErrClosed
		}
		names, err := f.namesLocked()
		if err != nil {
			return err
		}
		previousNeedsNewline := false
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			date, _ := logDate(name)
			if date < selection.From || date > selection.To {
				continue
			}
			file, err := f.root.Open(name)
			if err != nil {
				return err
			}
			info, err := file.Stat()
			if err != nil || !info.Mode().IsRegular() {
				_ = file.Close()
				if err != nil {
					return err
				}
				return errors.New("invalid log file")
			}
			f.refs[name]++
			if info.Size() > 0 && previousNeedsNewline {
				s.parts = append(s.parts, logPart{start: s.size, size: 1})
				s.size++
			}
			s.parts = append(s.parts, logPart{file: file, name: name, start: s.size, size: info.Size()})
			s.size += info.Size()
			if info.Size() > 0 {
				var last [1]byte
				if _, err := file.ReadAt(last[:], info.Size()-1); err != nil {
					return err
				}
				previousNeedsNewline = last[0] != '\n'
			}
		}
		if len(s.parts) == 0 {
			return os.ErrNotExist
		}
		return nil
	}()
	if err == nil && selection.Tail > 0 {
		s.offset, err = tailOffset(ctx, s, s.size, selection.Tail)
	}
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// tailOffset scans fixed-size blocks backwards. Memory does not depend on line length.
func tailOffset(ctx context.Context, r io.ReaderAt, size int64, lines int) (int64, error) {
	if size == 0 {
		return 0, nil
	}
	var last [1]byte
	if _, err := r.ReadAt(last[:], size-1); err != nil {
		return 0, err
	}
	end := size
	if last[0] == '\n' {
		end--
	}
	buf := make([]byte, 32*1024)
	for end > 0 {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		start := max(int64(0), end-int64(len(buf)))
		chunk := buf[:end-start]
		if _, err := r.ReadAt(chunk, start); err != nil {
			return 0, err
		}
		for i := len(chunk) - 1; i >= 0; i-- {
			if chunk[i] == '\n' {
				lines--
				if lines == 0 {
					return start + int64(i) + 1, nil
				}
			}
		}
		end = start
	}
	return 0, nil
}

func (s *logSnapshot) ReadAt(p []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, errors.New("negative offset")
	}
	n := 0
	index := sort.Search(len(s.parts), func(i int) bool { return s.parts[i].start+s.parts[i].size > offset })
	for len(p) > 0 && index < len(s.parts) {
		if err := s.ctx.Err(); err != nil {
			return n, err
		}
		part := s.parts[index]
		count := min(int64(len(p)), part.start+part.size-offset)
		if count > 0 {
			if part.file == nil {
				p[0] = '\n'
			} else {
				read, err := part.file.ReadAt(p[:count], offset-part.start)
				if err != nil {
					return n + read, err
				}
			}
			n += int(count)
			p = p[count:]
			offset += count
		}
		index++
	}
	if len(p) > 0 {
		return n, io.EOF
	}
	return n, nil
}

func (s *logSnapshot) Read(p []byte) (int, error) {
	// Bound each filesystem read so cancellation is observed during large transfers.
	if len(p) > 32*1024 {
		p = p[:32*1024]
	}
	n, err := s.ReadAt(p, s.offset)
	s.offset += int64(n)
	return n, err
}

func (s *logSnapshot) Close() error {
	s.once.Do(func() {
		s.owner.mu.Lock()
		defer s.owner.mu.Unlock()
		for _, part := range s.parts {
			if part.file != nil {
				_ = part.file.Close()
				s.owner.refs[part.name]--
				if s.owner.refs[part.name] == 0 {
					delete(s.owner.refs, part.name)
				}
			}
		}
	})
	return nil
}
