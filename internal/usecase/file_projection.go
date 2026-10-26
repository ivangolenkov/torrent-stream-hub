package usecase

import (
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
)

// This projection is shared by details, files, full SSE and compact summaries.
// It uses only immutable snapshots; rendering never performs a stat or hash.
func projectFileStates(t, persisted *models.Torrent, attached bool) {
	if !attached {
		t.Resume = nil
	}
	t.LastKnownBytes, t.LastKnownAtMS, t.LastKnownSource = persisted.Downloaded, persisted.ProgressObservedAt/1e6, persisted.ProgressSource
	saved := make(map[int]*models.File, len(persisted.Files))
	for _, f := range persisted.Files {
		saved[f.Index] = f
	}
	selection := &models.SelectionProgress{CatalogVersion: t.CatalogVersion}
	if t.Control != nil {
		selection.Revision = t.Control.DesiredRevision
	}
	allSelectedReady := true
	allFilesReady := len(t.Files) > 0
	for _, f := range t.Files {
		status := models.FileStatus{Presence: "unknown", Verification: "unknown", ReadReason: "runtime_not_attached", EffectivePriority: "none"}
		if attached && f.Status != nil {
			status = *f.Status
		}
		if old := saved[f.Index]; old != nil && old.Path == f.Path && old.Size == f.Size {
			status.LastKnownBytes = old.Downloaded
			status.LastKnownAtMS, status.LastKnownSource = persisted.ProgressObservedAt/1e6, persisted.ProgressSource
			if f.MetadataPath == "" {
				f.MetadataPath = old.MetadataPath
			}
			if old.Status != nil {
				status.Presence, status.Reason, status.ObservedAtMS = old.Status.Presence, old.Status.Reason, old.Status.ObservedAtMS
				status.StoragePath = old.Status.StoragePath
			}
		}
		if s := t.Storage; s != nil {
			if f.MetadataPath != "" {
				if p, err := (payload.Layout{Version: s.LayoutVersion, Root: s.Root, Prefix: s.Prefix}).Path(f.MetadataPath); err == nil {
					status.StoragePath = p
				}
			}
			if s.Inventory != "ready" && s.Inventory != "observing" {
				status.DirectReadReady = false
				status.ReadReason = "inventory_pending"
				if s.InventoryReason != "" {
					status.ReadReason = s.InventoryReason
				}
				if s.Inventory == "pending" || s.Inventory == "unknown" {
					status.Presence, status.Reason = "unknown", "inventory_pending"
				}
				if s.InventoryReason == "volume_unavailable" || s.InventoryReason == "volume_unregistered" {
					status.Presence, status.Reason = "unavailable", s.InventoryReason
				}
			} else if attached {
				if status.VerifiedBytes > 0 {
					status.Presence, status.Reason = "present", ""
					if t.ProgressObservedAt > 0 {
						status.ObservedAtMS = t.ProgressObservedAt / 1e6
					}
				}
				// Zero-length files need actual rooted-open evidence from this
				// inventory session; an empty metadata object is insufficient.
				if f.Size == 0 && status.Presence == "present" && status.Verification == "completed" && t.ProgressSource == "verified" {
					status.DirectReadReady, status.ReadReason = true, ""
				}
			}
			if s.Status != "ready" {
				status.DirectReadReady, status.ReadReason = false, "storage_unavailable"
			}
			if op := s.Operation; op != nil {
				switch op.Stage {
				case "planning", "planned", "analyzing_location", "analyzed", "completed", "cancelled":
				default:
					status.DirectReadReady, status.ReadReason = false, "storage_operation"
				}
			}
		}
		if !attached {
			status.VerifiedBytes, status.CheckedBytes, status.DirectReadReady = 0, 0, false
		}
		if t.DeletionMode != "" {
			status.DirectReadReady, status.ReadReason = false, "deleting"
		}
		if t.Control != nil {
			for _, b := range t.Control.ActivationBlocks {
				if b.Kind == models.BlockVerification || b.Kind == models.BlockMissingFiles {
					status.DirectReadReady, status.ReadReason = false, string(b.Kind)
				}
			}
		}
		if status.DirectReadReady {
			status.ReadReason = ""
		}
		f.Status = &status
		allFilesReady = allFilesReady && status.DirectReadReady
		if f.Priority != models.PriorityNone {
			selection.Files++
			selection.Size += f.Size
			selection.VerifiedBytes += max(int64(0), min(status.VerifiedBytes, f.Size))
			allSelectedReady = allSelectedReady && status.DirectReadReady
		}
	}
	if selection.Size > 0 {
		p := models.ProgressPercent(selection.VerifiedBytes, selection.Size)
		selection.Progress = &p
	}
	selection.Complete = attached && selection.Files > 0 && allSelectedReady
	t.Selection = selection
	t.DirectReadReady = attached && allFilesReady
	if !attached {
		t.Verification = nil
	}
}
