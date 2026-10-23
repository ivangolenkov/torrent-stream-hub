package engine

import "torrent-stream-hub/internal/models"

// RuntimeReadStamp fences application-owned runtime facts, not continually
// changing library telemetry (bytes/peers/speeds) or stream accounting samples.
// No stamp holds any lock across a repository or library call.
type RuntimeReadStamp struct {
	all     bool
	hash    string
	entries map[string]runtimeReadEntry
}

type runtimeReadEntry struct {
	owner   *ManagedTorrent
	epoch   uint64
	fence   models.RuntimeFence
	applied int64
	failure *models.IntentApplyFailure
}

func readEntry(mt *ManagedTorrent) runtimeReadEntry {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	return runtimeReadEntry{owner: mt, epoch: mt.policyEpoch, fence: mt.runtimeFence, applied: mt.appliedIntentRevision, failure: mt.intentApplyFailure}
}

// An empty hash captures map membership as well as the identities of its entries.
// A single-hash stamp also represents absence, so delete/re-add cannot pass by ABA.
func (e *Engine) CaptureReadStamp(hash string) RuntimeReadStamp {
	s := RuntimeReadStamp{all: hash == "", hash: hash, entries: make(map[string]runtimeReadEntry)}
	e.mu.RLock()
	defer e.mu.RUnlock()
	for key, mt := range e.managedTorrents {
		if s.all || key == hash {
			s.entries[key] = readEntry(mt)
		}
	}
	return s
}

func (e *Engine) ValidateReadStamp(s RuntimeReadStamp) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if s.all && len(s.entries) != len(e.managedTorrents) {
		return false
	}
	if !s.all && len(s.entries) == 0 {
		return e.managedTorrents[s.hash] == nil
	}
	for key, entry := range s.entries {
		mt := e.managedTorrents[key]
		if mt == nil || mt != entry.owner || readEntry(mt) != entry {
			return false
		}
	}
	return true
}
