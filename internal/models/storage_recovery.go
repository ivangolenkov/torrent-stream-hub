package models

// StorageRecovery is an additive diagnostic contract, not authorization to read
// payload or execute a command. Writers must revalidate RecordID and Version.
// It is populated by recovery workflows; absence means unavailable diagnostics,
// not proof that storage is healthy.
type StorageRecovery struct {
	RecordID          string                  `json:"record_id"`
	Version           int64                   `json:"version,string"`
	Reason            StorageRecoveryReason   `json:"reason"`
	Actions           []StorageRecoveryAction `json:"actions"`
	Runtime           string                  `json:"runtime"`
	DataReadiness     string                  `json:"data_readiness"`
	MetadataPath      string                  `json:"metadata_path,omitempty"`
	Deletion          *StorageDeletionStatus  `json:"deletion,omitempty"`
	Progress          *StorageProgress        `json:"progress,omitempty"`
	Analysis          *LegacyAnalysis         `json:"analysis,omitempty"`
	AlternativeCopies int                     `json:"alternative_copies,omitempty"`
	AlternativeBytes  int64                   `json:"alternative_bytes,omitempty"`
	FailureCode       string                  `json:"failure_code,omitempty"`
	RepeatCount       int64                   `json:"repeat_count,omitempty"`
}

type StorageRecoveryReason string

const (
	StorageLegacyConflict    StorageRecoveryReason = "legacy_file_conflict"
	StorageDeletionPending   StorageRecoveryReason = "deletion_pending"
	StorageDeletionCancelled StorageRecoveryReason = "deletion_cancelled"
	StorageDeletionMode      StorageRecoveryReason = "deletion_mode_conflict"
	StorageCatalogChanged    StorageRecoveryReason = "ownership_catalog_changed"
	StorageOwnershipBlocked  StorageRecoveryReason = "ownership_conflict"
	StorageMigrationQueued   StorageRecoveryReason = "migration_queued"
	StorageMigrationActive   StorageRecoveryReason = "migration_active"
	StorageAttachmentFailed  StorageRecoveryReason = "attachment_failed"
	StorageInsufficientSpace StorageRecoveryReason = "disk_full"
	StorageRecoveryComplete  StorageRecoveryReason = "recovery_complete"
)

type StorageRecoveryAction string

const (
	StorageAnalyzeLegacy  StorageRecoveryAction = "analyze_legacy"
	StorageApplyLegacy    StorageRecoveryAction = "apply_legacy"
	StorageRetryDeletion  StorageRecoveryAction = "retry_deletion"
	StorageCancelDeletion StorageRecoveryAction = "cancel_deletion"
	StorageForgetRecord   StorageRecoveryAction = "forget_record"
	StorageRetryOwnership StorageRecoveryAction = "retry_ownership"
	StorageResumeVerify   StorageRecoveryAction = "resume_verify"
)

// StorageActionError extends existing {error: string} responses without making
// clients parse error text. HTTP status selection remains the caller's job:
// unexpected database/I/O failures must not be disguised as conflicts.
type StorageActionError struct {
	Error           string                `json:"error"`
	Code            StorageRecoveryReason `json:"code"`
	StorageRecovery *StorageRecovery      `json:"storage_recovery"`
}

type LegacyCandidateEvidence struct {
	Size        int64 `json:"size"`
	ValidPieces int   `json:"valid_pieces"`
	TotalPieces int   `json:"total_pieces"`
}
type LegacyAnalysis struct {
	Path        string                  `json:"path"`
	Equal       bool                    `json:"equal"`
	Recommended string                  `json:"recommended,omitempty"`
	Final       LegacyCandidateEvidence `json:"final"`
	Part        LegacyCandidateEvidence `json:"part"`
	Limitations []string                `json:"limitations"`
}

// RemovedData is unknown for old manifests. Neither cancellation nor a missing
// file proves that all original data survived (or was removed by this worker).
type StorageDeletionStatus struct {
	Mode        string `json:"mode"`
	Stage       string `json:"stage"`
	RemovedData string `json:"removed_data"`
	Error       string `json:"error,omitempty"`
}

// StorageProgress is observational and scoped to one record/operation. Totals
// are pointers: unknown is omitted; a known empty stage explicitly has total 0.
// Timestamps use Unix milliseconds; LastProgressAt does not advance on heartbeat.
type StorageProgress struct {
	Stage          string `json:"stage"`
	QueuePosition  *int64 `json:"queue_position,omitempty"`
	BytesDone      int64  `json:"bytes_done"`
	BytesTotal     *int64 `json:"bytes_total,omitempty"`
	FilesDone      int64  `json:"files_done"`
	FilesTotal     *int64 `json:"files_total,omitempty"`
	FilePath       string `json:"file_path,omitempty"`
	FileBytesDone  int64  `json:"file_bytes_done"`
	FileBytesTotal *int64 `json:"file_bytes_total,omitempty"`
	StageStartedAt int64  `json:"stage_started_at_ms"`
	LastProgressAt int64  `json:"last_progress_at_ms"`
	HeartbeatAt    int64  `json:"heartbeat_at_ms"`
}
