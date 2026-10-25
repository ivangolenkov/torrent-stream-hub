package repository

import (
	"encoding/json"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
)

func loadStorageLocations(tx sqlReader, torrents map[string]*models.Torrent) error {
	rows, err := tx.Query(`SELECT s.hash,s.storage_id,s.revision,s.version,s.root,s.prefix,s.state,s.error,COALESCE(i.status,'unknown'),COALESCE(i.reason,''),COALESCE(i.observed_at_ms,0) FROM torrent_storage s LEFT JOIN storage_inventory i ON i.storage_id=s.storage_id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var hash string
		v := new(models.StorageLocation)
		if err := rows.Scan(&hash, &v.ID, &v.Revision, &v.LayoutVersion, &v.Root, &v.Prefix, &v.Status, &v.Reason, &v.Inventory, &v.InventoryReason, &v.InventoryObservedAt); err != nil {
			rows.Close()
			return err
		}
		if t := torrents[hash]; t != nil {
			if v.Status == "ready" && (payload.Layout{Version: v.LayoutVersion, Root: v.Root, Prefix: v.Prefix}).Validate() != nil {
				v.Status = "blocked"
				v.Reason = "unsupported_layout"
			}
			t.Storage = v
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = tx.Query(`SELECT hash,version,reason,stage,error,failure_code,location_source,location_target,location_method,location_extra_bytes,location_analysis,location_free_bytes FROM storage_recovery WHERE reason IN ('storage_move','storage_locate')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var hash, reason, analysis string
		op := new(models.StorageOperation)
		if err := rows.Scan(&hash, &op.Version, &reason, &op.Stage, &op.Error, &op.FailureCode, &op.Source, &op.Target, &op.Method, &op.ExtraBytes, &analysis, &op.FreeBytes); err != nil {
			return err
		}
		t := torrents[hash]
		if t == nil || t.Storage == nil {
			continue
		}
		if analysis != "" {
			if err := json.Unmarshal([]byte(analysis), &op.Analysis); err != nil {
				return err
			}
		}
		op.Kind = "move"
		if reason == "storage_locate" {
			op.Kind = "locate"
		}
		op.Actions = []string{}
		switch op.Stage {
		case "planned":
			op.Actions = []string{"apply_move", "cancel_storage"}
		case "analyzed":
			op.Actions = []string{"cancel_storage"}
			if a := op.Analysis; a != nil && (a.ValidPieces > 0 || (a.TotalPieces == 0 && a.FoundFiles > 0 && a.MissingFiles == 0)) {
				op.Actions = append([]string{"apply_location"}, op.Actions...)
			}
		case "completed", "cancelled":
		case "publishing", "published", "committed", "cleanup":
			if op.Error != "" {
				op.Actions = []string{"retry_storage"}
			}
		default:
			op.Actions = []string{"cancel_storage"}
			if op.Error != "" {
				op.Actions = append(op.Actions, "retry_storage")
			}
		}
		t.Storage.Operation = op
	}
	return rows.Err()
}
