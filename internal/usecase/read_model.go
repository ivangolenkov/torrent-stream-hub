package usecase

import "torrent-stream-hub/internal/models"

// projectTorrent is shared by REST and SSE. A live engine entry is never a
// substitute for a committed record. Old record identities cannot contribute
// stats/readiness to a re-add with the same infohash.
func projectTorrent(runtime, persisted *models.Torrent) *models.Torrent {
	if persisted == nil {
		return nil
	}
	if persisted.Control == nil {
		return persisted
	}
	if runtime == nil {
		return projectDetachedTorrent(persisted)
	}
	rc := runtime.Control
	if rc != nil && rc.RecordID != "" && rc.RecordID != persisted.Control.RecordID {
		return projectDetachedTorrent(persisted)
	}
	if rc != nil && rc.AppliedRevision > persisted.Control.DesiredRevision {
		return projectDetachedTorrent(persisted)
	}
	mergePersistedMetadata(runtime, persisted)
	c := *persisted.Control
	c.ActivationBlocks = append([]models.ActivationBlock{}, c.ActivationBlocks...)
	if rc != nil && rc.RecordID == c.RecordID && rc.RuntimeAttachment != "" {
		c.RuntimeAttachment = rc.RuntimeAttachment
		c.AppliedRevision = rc.AppliedRevision
		c.ReconcilePending = c.AppliedRevision != c.DesiredRevision
		c.ReconcileReason = ""
		if c.ReconcilePending {
			c.ReconcileReason = "intent_not_applied"
		}
	}
	if rc != nil && rc.ApplyFailure != nil && rc.ApplyFailure.RecordID == c.RecordID && rc.ApplyFailure.Revision <= c.DesiredRevision {
		failure := *rc.ApplyFailure
		c.ApplyFailure = &failure
		c.ReconcilePending = true
		c.ReconcileReason = failure.Code
	}
	runtime.Control = &c
	return runtime
}

// No live attachment can contribute an operational phase. Use the same durable
// projection as restore, not historical Downloading/Seeding or an arbitrary
// observation error. Last-known bytes retain their existing provenance fields.
func projectDetachedTorrent(t *models.Torrent) *models.Torrent {
	t.Activity = models.TorrentActivity{}
	if c := t.Control; c != nil && c.DesiredRun.Valid() {
		t.State, t.Error = restoreActivation(c.DesiredRun, t.DownloadBlock, c.ActivationBlocks)
	}
	return t
}

func projectTorrents(runtime, persisted []*models.Torrent) []*models.Torrent {
	byHash := make(map[string]*models.Torrent, len(runtime))
	for _, t := range runtime {
		byHash[t.Hash] = t
	}
	out := make([]*models.Torrent, 0, len(persisted))
	for _, t := range persisted {
		out = append(out, projectTorrent(byHash[t.Hash], t))
	}
	return out
}
