package engine

import (
	"sort"

	"github.com/anacrolix/torrent"
)

// A prefix over state runs avoids one entry per piece and repeated scans per
// file. File intersections exclude shared boundary bytes and v2 padding gaps.
func verifiedPrefix(runs torrent.PieceStateRuns, pieceLength int64) func(int64) int64 {
	type interval struct {
		start, end, before int64
		complete           bool
	}
	intervals := make([]interval, 0, len(runs))
	var offset, total int64
	for _, run := range runs {
		end := offset + int64(run.Length)*pieceLength
		complete := run.Ok && run.Complete && !run.Checking && !run.Marking
		intervals = append(intervals, interval{offset, end, total, complete})
		if complete {
			total += end - offset
		}
		offset = end
	}
	return func(pos int64) int64 {
		if pos <= 0 || len(intervals) == 0 {
			return 0
		}
		i := sort.Search(len(intervals), func(i int) bool { return intervals[i].end >= pos })
		if i == len(intervals) {
			return total
		}
		r := intervals[i]
		if r.complete {
			return r.before + pos - r.start
		}
		return r.before
	}
}
