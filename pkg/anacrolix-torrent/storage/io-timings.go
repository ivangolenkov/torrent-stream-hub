package storage

import (
	"sync/atomic"
	"time"
)

// IOTimings are cumulative, attachment-local diagnostics, not readiness or
// durability evidence. Totals count concurrent operation time, not wall time.
// Snapshots are lock-free and approximate across fields. No paths are exposed.
// A completed operation may measure zero nanoseconds at platform clock resolution;
// counts/activity, not strictly positive duration, identify observed operations.
type IOTimings struct {
	CompletionSyncCount, CompletionSyncErrors, CompletionSyncActive int64
	CompletionSyncNS, CompletionSyncMaxNS                           int64
	WriteLeaseWaitCount, WriteLeaseWaitActive                       int64
	WriteLeaseWaitNS, WriteLeaseWaitMaxNS                           int64
	WriteLeaseHold, WritePrepare, WriteGuard, WriteOpen             IOStageTimings
	WriteStat, WritePayload, WriteAccount, WriteClose               IOStageTimings
}

// Fixed-size per-stage diagnostics. Counts are stage invocations, not chunks;
// Stat is sampled before and after each contributing-file write.
type IOStageTimings struct {
	Count, Errors, Active, NS, MaxNS int64
}

type ioStageTimings struct {
	count, errors, active, ns, maxNS atomic.Int64
}

func (s *ioStageTimings) begin() time.Time {
	s.active.Add(1)
	return time.Now()
}

func (s *ioStageTimings) end(start time.Time, err error) {
	ns := time.Since(start).Nanoseconds()
	s.ns.Add(ns)
	recordIOMax(&s.maxNS, ns)
	if err != nil {
		s.errors.Add(1)
	}
	s.count.Add(1)
	s.active.Add(-1)
}

func (s *ioStageTimings) snapshot() IOStageTimings {
	return IOStageTimings{Count: s.count.Load(), Errors: s.errors.Load(), Active: s.active.Load(), NS: s.ns.Load(), MaxNS: s.maxNS.Load()}
}

type ioTimings struct {
	syncCount, syncErrors, syncActive, syncNS, syncMaxNS         atomic.Int64
	writeWaitCount, writeWaitActive, writeWaitNS, writeWaitMaxNS atomic.Int64
	writeLeaseHold, writePrepare, writeGuard, writeOpen          ioStageTimings
	writeStat, writePayload, writeAccount, writeClose            ioStageTimings
}

func (s *ioTimings) snapshot() IOTimings {
	return IOTimings{
		CompletionSyncCount: s.syncCount.Load(), CompletionSyncErrors: s.syncErrors.Load(),
		CompletionSyncActive: s.syncActive.Load(), CompletionSyncNS: s.syncNS.Load(), CompletionSyncMaxNS: s.syncMaxNS.Load(),
		WriteLeaseWaitCount: s.writeWaitCount.Load(), WriteLeaseWaitActive: s.writeWaitActive.Load(),
		WriteLeaseWaitNS: s.writeWaitNS.Load(), WriteLeaseWaitMaxNS: s.writeWaitMaxNS.Load(),
		WriteLeaseHold: s.writeLeaseHold.snapshot(), WritePrepare: s.writePrepare.snapshot(),
		WriteGuard: s.writeGuard.snapshot(), WriteOpen: s.writeOpen.snapshot(),
		WriteStat: s.writeStat.snapshot(), WritePayload: s.writePayload.snapshot(),
		WriteAccount: s.writeAccount.snapshot(), WriteClose: s.writeClose.snapshot(),
	}
}

func recordIOMax(dst *atomic.Int64, ns int64) {
	for old := dst.Load(); ns > old; old = dst.Load() {
		if dst.CompareAndSwap(old, ns) {
			return
		}
	}
}

func (p *filePieceImpl) syncCompletion() error {
	s := &p.t.ioTimings
	start := time.Now()
	s.syncActive.Add(1)
	err := p.Flush()
	ns := time.Since(start).Nanoseconds()
	s.syncNS.Add(ns)
	recordIOMax(&s.syncMaxNS, ns)
	if err != nil {
		s.syncErrors.Add(1)
	}
	s.syncCount.Add(1)
	s.syncActive.Add(-1)
	return err
}
