package engine

import (
	"context"
	"fmt"
	"sort"
	"time"

	"torrent-stream-hub/internal/logging"
)

// resourceMonitor checks disk space and manages active downloads limit.
func (e *Engine) resourceMonitor(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	e.manageResources()

	for {
		select {
		case <-ctx.Done():
			logging.Debugf("resource monitor stopped")
			return
		case <-ticker.C:
			e.manageResources()
		}
	}
}

func (e *Engine) manageResources() {
	if !e.beginWork() {
		return
	}
	defer e.wg.Done()
	e.admissionMu.Lock()
	minFreeBytes := e.minimumDiskBytes()
	e.admissionMu.Unlock()
	// Global filesystem health is diagnostic. Per-runtime admission does its own
	// probe under a fenced preparation ticket, never while holding admissionMu.
	freeSpace, err := GetFreeSpace(e.cfg.DownloadDir)
	if err != nil {
		logging.Warnf("failed to check free disk space dir=%s: %v", e.cfg.DownloadDir, err)
	}
	diskFull := err != nil || freeSpace < minFreeBytes

	e.mu.RLock()
	torrents := make([]*ManagedTorrent, 0, len(e.managedTorrents))
	for _, mt := range e.managedTorrents {
		torrents = append(torrents, mt)
	}
	totalTorrents := len(e.managedTorrents)
	e.mu.RUnlock()
	// Copy sort keys under their owner lock; don't mutate them in the comparator.
	type ordered struct {
		mt       *ManagedTorrent
		position int64
		added    time.Time
	}
	order := make([]ordered, 0, len(torrents))
	for _, mt := range torrents {
		if mt != nil {
			mt.mu.Lock()
			order = append(order, ordered{mt, mt.queuePosition, mt.addedAt})
			mt.mu.Unlock()
		}
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if a.position > 0 && b.position > 0 && a.position != b.position {
			return a.position < b.position
		}
		if a.position > 0 != (b.position > 0) {
			return a.position > 0
		}
		if !a.added.Equal(b.added) {
			return a.added.Before(b.added)
		}
		return a.mt.hash < b.mt.hash
	})
	torrents = torrents[:0]
	for _, entry := range order {
		torrents = append(torrents, entry.mt)
	}

	for _, mt := range torrents {
		if mt == nil || mt.t == nil {
			continue
		}
		e.admissionMu.Lock()
		if !e.currentTorrent(mt) {
			e.admissionMu.Unlock()
			continue
		}
		// Coalesce health probes per attachment. A blocked probe for A must not
		// make every other scheduler pass wait for A before it can process B.
		if mt.healthProbe != nil {
			e.admissionMu.Unlock()
			continue
		}
		mt.mu.Lock()
		probeTicket := &payloadPreparation{epoch: mt.policyEpoch, fence: mt.runtimeFence}
		mt.mu.Unlock()
		mt.healthProbe = probeTicket
		probe, minimum := e.diskProbe, e.minimumDiskBytes()
		e.admissionMu.Unlock()
		// The generation owns a probe ticket before any potentially slow stat.
		mt.t.RefreshStorageCompletion()
		probeErr := e.diskErrorWithProbe(mt.hash, probe, minimum)
		e.admissionMu.Lock()
		mt.mu.Lock()
		valid := mt.policyEpoch == probeTicket.epoch && mt.runtimeFence == probeTicket.fence
		mt.mu.Unlock()
		valid = valid && e.currentTorrent(mt) && mt.healthProbe == probeTicket
		if mt.healthProbe == probeTicket {
			mt.healthProbe = nil
		}
		if !valid {
			e.admissionMu.Unlock()
			continue
		}
		if probeErr != nil {
			_ = e.blockDownloadLocked(mt, probeErr.Error())
			e.admissionMu.Unlock()
			continue
		}
		e.reconcileSelectionLocked(mt)
		mt.mu.Lock()
		reserved := mt.policy.slot
		mt.mu.Unlock()
		if !reserved {
			// Recount actual reservations after every unlocked preparation; a
			// concurrent scheduler pass may have reserved capacity in the meantime.
			available := e.slotCountLocked() < e.cfg.MaxActiveDownloads && e.schedulerHeadLocked() == mt
			mt.mu.Lock()
			_, grantErr := mt.applyStateEventLocked(stateEvent{kind: stateSlotRequested, slotAvailable: available})
			mt.mu.Unlock()
			if grantErr != nil {
				e.admissionMu.Unlock()
				continue
			}
		}
		mt.mu.Lock()
		token := mt.slotToken
		prepare := mt.policy.slot && mt.policy.payloadEligible() && !mt.downloadAllStarted && mt.preparation == nil && mt.t.Info() != nil
		if prepare {
			mt.downloadAllStarted = true
		}
		mt.mu.Unlock()
		if prepare {
			if admitErr := e.admitPayloadLocked(mt); admitErr != nil {
				mt.mu.Lock()
				owned := mt.policy.slot && mt.slotToken == token
				if owned {
					mt.downloadAllStarted = false
				}
				mt.mu.Unlock()
				if owned {
					e.releaseFailedSlotLocked(mt)
				}
			}
		}
		e.admissionMu.Unlock()
	}
	e.admissionMu.Lock()
	activeCount := e.slotCountLocked()
	e.admissionMu.Unlock()
	e.logResourceSummary(totalTorrents, activeCount, diskFull, freeSpace, err)
}

// admissionMu is held. Pending preparations count as reserved, not free slots.
func (e *Engine) slotCountLocked() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	n := 0
	for _, mt := range e.managedTorrents {
		mt.mu.Lock()
		if mt.policy.slot {
			n++
		}
		mt.mu.Unlock()
	}
	return n
}

// Re-evaluate FIFO under reservation serialization, not the earlier sort snapshot.
// A concurrent Add/priority/queue update cannot grant a later candidate merely
// because an older pass captured a now-obsolete ordering.
func (e *Engine) schedulerHeadLocked() *ManagedTorrent {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var first *ManagedTorrent
	var position int64
	var added time.Time
	for _, mt := range e.managedTorrents {
		mt.mu.Lock()
		eligible := mt.policy.payloadEligible() && !mt.policy.slot && !mt.policy.selectedComplete && !mt.policy.payloadComplete
		p, a := mt.queuePosition, mt.addedAt
		mt.mu.Unlock()
		if !eligible {
			continue
		}
		before := first == nil
		if first != nil {
			switch {
			case p > 0 && position > 0 && p != position:
				before = p < position
			case (p > 0) != (position > 0):
				before = p > 0
			case !a.Equal(added):
				before = a.Before(added)
			default:
				before = mt.hash < first.hash
			}
		}
		if before {
			first, position, added = mt, p, a
		}
	}
	return first
}

// An admission error may already have installed Error/DiskFull. Only the still
// owned Downloading slot can be released; never erase the actual failure phase.
func (e *Engine) releaseFailedSlotLocked(mt *ManagedTorrent) {
	if !e.currentTorrent(mt) {
		return
	}
	mt.mu.Lock()
	d, err := mt.applyStateEventLocked(stateEvent{kind: stateSlotReleased})
	mt.mu.Unlock()
	if err == nil {
		e.applyBackgroundReleaseLocked(mt, d)
	}
}

func (e *Engine) logResourceSummary(totalTorrents, activeCount int, diskFull bool, freeSpace uint64, freeSpaceErr error) {
	if !logging.IsDebugEnabled() {
		return
	}

	freeValue := "unknown"
	if freeSpaceErr == nil {
		freeValue = fmt.Sprintf("%d", freeSpace)
	}
	key := fmt.Sprintf("torrents=%d active=%d max=%d disk_full=%t free=%s", totalTorrents, activeCount, e.cfg.MaxActiveDownloads, diskFull, freeValue)
	now := time.Now()

	e.resourceLogMu.Lock()
	defer e.resourceLogMu.Unlock()
	if key == e.lastResourceKey && !e.lastResourceLog.IsZero() && now.Sub(e.lastResourceLog) < 30*time.Second {
		return
	}

	logging.Debugf("resource manager %s", key)
	e.lastResourceKey = key
	e.lastResourceLog = now
}
