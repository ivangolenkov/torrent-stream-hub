package models

// StorageLocation is an observational projection, never authority to access a
// path. Record, mapping and operation versions are independently fenced.
type StorageLocation struct {
	Inventory           string            `json:"inventory,omitempty"`
	InventoryReason     string            `json:"inventory_reason,omitempty"`
	InventoryObservedAt int64             `json:"inventory_observed_at_ms,omitempty"`
	ID                  string            `json:"id"`
	Revision            int64             `json:"revision,string"`
	LayoutVersion       int               `json:"layout_version"`
	Root                string            `json:"root"`
	Prefix              string            `json:"prefix"`
	Status              string            `json:"status"`
	Reason              string            `json:"reason,omitempty"`
	Operation           *StorageOperation `json:"operation,omitempty"`
}

type StorageOperation struct {
	FreeBytes   *uint64           `json:"free_bytes,omitempty"`
	Analysis    *LocationAnalysis `json:"analysis,omitempty"`
	Kind        string            `json:"kind"`
	Stage       string            `json:"stage"`
	Version     int64             `json:"version,string"`
	Source      string            `json:"source"`
	Target      string            `json:"target"`
	Actions     []string          `json:"actions"`
	Method      string            `json:"method,omitempty"`
	ExtraBytes  int64             `json:"extra_bytes,omitempty"`
	Error       string            `json:"error,omitempty"`
	FailureCode string            `json:"failure_code,omitempty"`
}

type LocationAnalysis struct {
	FoundFiles   int      `json:"found_files"`
	MissingFiles int      `json:"missing_files"`
	ValidPieces  int      `json:"valid_pieces"`
	TotalPieces  int      `json:"total_pieces"`
	Limitations  []string `json:"limitations"`
}

type FileStatus struct {
	ReadReason          string `json:"read_reason,omitempty"`
	LastKnownAtMS       int64  `json:"last_known_at_ms,omitempty"`
	LastKnownSource     string `json:"last_known_source,omitempty"`
	CheckedBytes        int64  `json:"checked_bytes,omitempty"`
	SkippedMissingBytes int64  `json:"skipped_missing_bytes,omitempty"`
	VerificationTotal   int64  `json:"verification_total,omitempty"`
	StoragePath         string `json:"storage_path"`
	Presence            string `json:"presence"`
	Reason              string `json:"reason,omitempty"`
	VerifiedBytes       int64  `json:"verified_bytes"`
	LastKnownBytes      int64  `json:"last_known_bytes"`
	ObservedAtMS        int64  `json:"observed_at_ms,omitempty"`
	Verification        string `json:"verification"`
	DirectReadReady     bool   `json:"direct_read_ready"`
	EffectivePriority   string `json:"effective_priority"`
}

type SelectionProgress struct {
	CatalogVersion string   `json:"catalog_version,omitempty"`
	Revision       int64    `json:"revision,string,omitempty"`
	Complete       bool     `json:"complete,omitempty"`
	Size           int64    `json:"size"`
	VerifiedBytes  int64    `json:"verified_bytes"`
	Progress       *float64 `json:"progress,omitempty"`
	Files          int      `json:"files"`
}

// Checked bytes count completed hash attempts, including invalid data. They do
// not grant readiness. Attachment and operation distinguish a new pass.
type VerificationProgress struct {
	ProgressAtMS        int64  `json:"progress_at_ms,omitempty"`
	Attachment          string `json:"attachment,omitempty"`
	Operation           uint64 `json:"operation,omitempty"`
	Status              string `json:"status"`
	CheckedBytes        int64  `json:"checked_bytes"`
	SkippedMissingBytes int64  `json:"skipped_missing_bytes,omitempty"`
	TotalBytes          int64  `json:"total_bytes"`
	QueuedPieces        int    `json:"queued_pieces"`
	ActivePieces        int    `json:"active_pieces"`
}

type StorageCommand struct {
	Action          string `json:"action"`
	RecordID        string `json:"record_id"`
	StorageID       string `json:"storage_id"`
	MappingRevision int64  `json:"mapping_revision,string"`
	Version         int64  `json:"version,string,omitempty"`
	FolderName      string `json:"folder_name,omitempty"`
	Path            string `json:"path,omitempty"`
	StripPrefix     string `json:"strip_prefix,omitempty"`
}
