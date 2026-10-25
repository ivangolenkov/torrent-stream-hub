package models

// RunIntent is a durable command, not the observed operational phase. Enabled
// permits scheduler participation; it grants neither a slot nor file readiness.
type RunIntent string

const (
	RunPaused  RunIntent = "paused"
	RunEnabled RunIntent = "enabled"
)

func (v RunIntent) Valid() bool { return v == RunPaused || v == RunEnabled }

type IntentVersion struct {
	Hash     string
	RecordID string
	Revision int64
}

type TorrentIntent struct {
	Version IntentVersion
	Run     RunIntent
}

// RuntimeFence is a capability for a single attachment of a particular record.
// It must travel with the captured observation, never be looked up later by hash.
type RuntimeFence struct {
	IntentVersion
	Attachment string
}

type FileObservation struct {
	Index         int
	VerifiedBytes int64
}

type UserMetadata struct{ Title, Data, Poster, Category string }

// FileCatalog contains only metainfo-derived facts, never observed priorities.
type FileCatalog struct {
	MetadataPath string
	Index        int
	Path         string
	Size         int64
	Offset       int64
	IsMedia      bool
}

type TorrentCatalog struct {
	Format      string // "", v1, v2 or hybrid; empty is an unknown legacy layout.
	Name        string
	Size        int64
	PieceLength int64
	NumPieces   int
	Files       []FileCatalog
}

type NewTorrentRecord struct {
	Hash      string
	Name      string
	SourceURI string
	Run       RunIntent
	Temporary bool
	Metadata  UserMetadata
	Catalog   *TorrentCatalog
}

// RuntimeObservation deliberately has no user metadata, selection, temporary
// policy, source URI, queue position or workflow fields. Catalog ingestion is a
// separate operation; observations cannot create files or torrent records.
type RuntimeObservation struct {
	Fence           RuntimeFence
	Sequence        int64
	AppliedRevision int64
	State           TorrentState
	Error           ErrorReason
	ProgressSource  string
	ObservedAt      int64
	VerifiedBytes   int64
	Files           []FileObservation
}

type ActivationBlockKind string

type ActivationBlock struct {
	Kind   ActivationBlockKind `json:"kind"`
	Reason string              `json:"reason"`
}

// TorrentActivity is live attachment-scoped accounting, never persisted intent
// or proof of readiness. DirectReaders is separate from session accounting: one
// HTTP stream may own both a disk descriptor and an accounting-only session.
type TorrentActivity struct {
	Streaming     bool `json:"streaming"`
	Streams       int  `json:"streams"`
	DirectReaders int  `json:"direct_readers"`
	Preloads      int  `json:"preloads"`
	Debouncing    bool `json:"debouncing"`
	Verifying     bool `json:"verifying"`
}

// TorrentControl is a read projection, never an argument to an intent writer.
// Decimal strings preserve the complete SQLite revision range in JavaScript.
// AppliedRevision concerns this live attachment, not a prior process's snapshot.
type TorrentControl struct {
	RecordID          string              `json:"record_id"`
	DesiredRun        RunIntent           `json:"desired_run"`
	DesiredRevision   int64               `json:"desired_revision,string"`
	AppliedRevision   int64               `json:"applied_revision,string"`
	RuntimeAttachment string              `json:"runtime_attachment,omitempty"`
	ReconcilePending  bool                `json:"reconcile_pending"`
	ReconcileReason   string              `json:"reconcile_reason,omitempty"`
	ApplyFailure      *IntentApplyFailure `json:"apply_failure,omitempty"`
	ActivationBlocks  []ActivationBlock   `json:"activation_blocks"`
}

// IntentApplyFailure is bounded live-runtime diagnostics, not a durable safety
// block. Code names the failed stage, never raw filesystem/library error text.
// The command response retains the original error. Retry is explicit: no sampler
// or metadata command is authorized to reopen gates after a partial failure.
type IntentApplyFailure struct {
	RecordID string `json:"record_id"`
	Revision int64  `json:"revision,string"`
	Code     string `json:"code"`
	Retry    string `json:"retry"`
}

const (
	BlockVerification        ActivationBlockKind = "verification"
	BlockMissingFiles        ActivationBlockKind = "missing_files"
	BlockLegacyError         ActivationBlockKind = "legacy_error"
	BlockMetadataPersistence ActivationBlockKind = "metadata_persistence"
	BlockInvalidSelection    ActivationBlockKind = "invalid_selection"
)

func (k ActivationBlockKind) Valid() bool {
	return k == BlockVerification || k == BlockMissingFiles || k == BlockLegacyError || k == BlockMetadataPersistence || k == BlockInvalidSelection
}

// ResumeRetryable means explicit authorization to retry/repair with network
// access, not evidence that the old payload is present or verified.
func (k ActivationBlockKind) ResumeRetryable() bool {
	return k == BlockLegacyError || k == BlockMissingFiles || k == BlockMetadataPersistence || k == BlockInvalidSelection
}
