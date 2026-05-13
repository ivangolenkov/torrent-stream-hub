package torrserver

import (
	"strconv"
	"strings"
)

type parsedStreamRange struct {
	Start    int64
	End      int64
	HasRange bool
	Valid    bool
	Multi    bool
}

func parseStreamRange(header string, size int64) parsedStreamRange {
	header = strings.TrimSpace(header)
	if header == "" {
		return parsedStreamRange{Start: 0, End: maxInt64(size-1, -1), Valid: true}
	}
	if !strings.HasPrefix(strings.ToLower(header), "bytes=") {
		return parsedStreamRange{Valid: false}
	}
	spec := strings.TrimSpace(header[len("bytes="):])
	if strings.Contains(spec, ",") {
		return parsedStreamRange{HasRange: true, Valid: false, Multi: true}
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return parsedStreamRange{HasRange: true, Valid: false}
	}

	startText := strings.TrimSpace(spec[:dash])
	endText := strings.TrimSpace(spec[dash+1:])
	if startText == "" {
		suffix, err := strconv.ParseInt(endText, 10, 64)
		if err != nil || suffix <= 0 {
			return parsedStreamRange{HasRange: true, Valid: false}
		}
		if size <= 0 {
			return parsedStreamRange{Start: 0, End: -1, HasRange: true, Valid: true}
		}
		start := size - suffix
		if start < 0 {
			start = 0
		}
		return parsedStreamRange{Start: start, End: size - 1, HasRange: true, Valid: true}
	}

	start, err := strconv.ParseInt(startText, 10, 64)
	if err != nil || start < 0 {
		return parsedStreamRange{HasRange: true, Valid: false}
	}
	if size > 0 && start >= size {
		return parsedStreamRange{Start: start, End: size - 1, HasRange: true, Valid: false}
	}

	end := size - 1
	if endText != "" {
		end, err = strconv.ParseInt(endText, 10, 64)
		if err != nil || end < start {
			return parsedStreamRange{HasRange: true, Valid: false}
		}
		if size > 0 && end >= size {
			end = size - 1
		}
	}
	if size <= 0 && endText == "" {
		end = -1
	}
	return parsedStreamRange{Start: start, End: end, HasRange: true, Valid: true}
}
