package logging

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const maxLogSegment int64 = 999999999999

// Legacy files sort before numbered segments of the same day. Fixed-width
// suffixes avoid ambiguous namespace and restart collisions.
func logSegment(name string) (date string, segment int64, ok bool) {
	if !strings.HasPrefix(name, "hub-") || !strings.HasSuffix(name, ".log") || (len(name) != 18 && len(name) != 31) {
		return "", 0, false
	}
	date = name[4:14]
	d, err := time.Parse(dateLayout, date)
	if err != nil || d.Format(dateLayout) != date {
		return "", 0, false
	}
	if len(name) == 18 {
		return date, 0, true
	}
	if name[14] != '.' {
		return "", 0, false
	}
	for _, c := range name[15:27] {
		if c < '0' || c > '9' {
			return "", 0, false
		}
	}
	segment, err = strconv.ParseInt(name[15:27], 10, 64)
	return date, segment, err == nil && segment > 0
}

func (f *Files) appendTargetLocked(date string, size int64) (string, error) {
	if size > f.opts.MaxFileBytes {
		return "", errors.New("log record exceeds file limit")
	}
	names, err := f.namesLocked()
	if err != nil {
		return "", err
	}
	name := "hub-" + date + ".log"
	var seq int64
	for _, candidate := range names {
		d, n, _ := logSegment(candidate)
		if d == date && n >= seq {
			seq = n
			name = candidate
		}
	}
	info, err := f.root.Lstat(name)
	if err == nil && !info.Mode().IsRegular() {
		return "", errors.New("log target is not a regular file")
	}
	if err == nil && info.Size() > f.opts.MaxFileBytes-size {
		if seq == maxLogSegment {
			return "", errors.New("log segment namespace exhausted")
		}
		name = fmt.Sprintf("hub-%s.%012d.log", date, seq+1)
	}
	return name, nil
}

// reserveBytesLocked counts all managed files, including pinned and oversized
// legacy files. If protected data occupies the budget, append pauses instead of
// exceeding it or truncating a reader's snapshot.
func (f *Files) reserveBytesLocked(incoming int64) bool {
	now := f.opts.Now()
	if f.quotaPaused.Load() && now.Before(f.quotaRetryAt) {
		return false
	}
	names, err := f.namesLocked()
	if err != nil {
		f.reportLocked(err)
		return false
	}
	var total int64
	sizes := make(map[string]int64, len(names))
	for _, name := range names {
		info, err := f.root.Lstat(name)
		if err != nil {
			f.reportLocked(err)
			return false
		}
		if info.Size() > math.MaxInt64-total {
			f.reportLocked(errors.New("log storage size overflow"))
			return false
		}
		sizes[name] = info.Size()
		total += info.Size()
	}
	for _, name := range names {
		if total <= f.opts.MaxTotalBytes-incoming {
			break
		}
		if name == f.active || f.refs[name] > 0 {
			continue
		}
		if err := f.root.Remove(name); err != nil {
			f.reportLocked(err)
			continue
		}
		total -= sizes[name]
	}
	paused := total > f.opts.MaxTotalBytes-incoming
	f.quotaPaused.Store(paused)
	if paused {
		f.quotaRetryAt = now.Add(time.Minute)
	}
	return !paused
}

type StorageStats struct {
	Errors      uint64 `json:"errors"`
	LostRecords uint64 `json:"lost_records"`
	LostBytes   uint64 `json:"lost_bytes"`
	QuotaPaused bool   `json:"quota_paused"`
}

func (f *Files) StorageStats() StorageStats {
	return StorageStats{Errors: f.storageErrors.Load(), LostRecords: f.lostRecords.Load(), LostBytes: f.lostBytes.Load(), QuotaPaused: f.quotaPaused.Load()}
}
