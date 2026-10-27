package models

// TorrentState is the compatible operational phase projection. Streaming and
// verification activity are independent runtime axes, not mutually exclusive phases.
type TorrentState string

const (
	StateQueued       TorrentState = "Queued"
	StateDownloading  TorrentState = "Downloading"
	StateStreaming    TorrentState = "Streaming" // Legacy persisted/API alias; runtime streaming is session activity.
	StateSeeding      TorrentState = "Seeding"
	StatePaused       TorrentState = "Paused"
	StateError        TorrentState = "Error"
	StateMissingFiles TorrentState = "MissingFiles"
	StateDiskFull     TorrentState = "DiskFull"
)

// ErrorReason provides a detailed cause when a torrent enters the Error or DiskFull state
type ErrorReason string

const (
	// Legacy persisted/API diagnosis strings. New runtime selection failures use
	// ErrInvalidSelection; peer/tracker diagnostics do not imply invalid payload.
	ErrInvalidTorrent      ErrorReason = "Invalid torrent"
	ErrTrackerUnreachable  ErrorReason = "Tracker unreachable"
	ErrNoPeers             ErrorReason = "No peers"
	ErrDiskFull            ErrorReason = "Disk full" // Legacy fixed reason; live disk latches carry a specific reason.
	ErrMissingFiles        ErrorReason = "Missing files"
	ErrVerificationFailed  ErrorReason = "Verification failed"
	ErrActivationBlocked   ErrorReason = "Activation blocked"
	ErrMetadataPersistence ErrorReason = "metadata persistence failed"
	ErrInvalidSelection    ErrorReason = "Invalid file selection"
	ErrNone                ErrorReason = ""
)

// FilePriority dictates the download priority for specific files in a torrent
type FilePriority int

const (
	PriorityNormal FilePriority = 0
	PriorityHigh   FilePriority = 1
	PriorityNone   FilePriority = -1
)

func ValidFilePriority(priority FilePriority) bool {
	return priority == PriorityNormal || priority == PriorityHigh || priority == PriorityNone
}

// Torrent defines the core properties and state of a single torrent
type Torrent struct {
	Resume          *ResumeProgress       `json:"resume,omitempty"`
	DirectReadReady bool                  `json:"direct_read_ready"`
	Selection       *SelectionProgress    `json:"selection,omitempty"`
	Verification    *VerificationProgress `json:"verification,omitempty"`
	LastKnownBytes  int64                 `json:"last_known_bytes"`
	LastKnownAtMS   int64                 `json:"last_known_at_ms,omitempty"`
	LastKnownSource string                `json:"last_known_source,omitempty"`
	Storage         *StorageLocation      `json:"storage,omitempty"`
	Control         *TorrentControl       `json:"control,omitempty"`
	DeletionMode    string                `json:"deletion_mode,omitempty"`
	DeletionError   string                `json:"deletion_error,omitempty"`
	DownloadBlock   string                `json:"download_block,omitempty"`
	StorageState    string                `json:"storage_state,omitempty"`
	StorageError    string                `json:"storage_error,omitempty"`
	StorageRecovery *StorageRecovery      `json:"storage_recovery,omitempty"`
	IsTemporary     bool                  `json:"is_temporary"`
	Hash            string                `json:"hash"`
	Name            string                `json:"name"`
	Title           string                `json:"title,omitempty"`
	Data            string                `json:"data,omitempty"`
	Poster          string                `json:"poster,omitempty"`
	Category        string                `json:"category,omitempty"`
	Size            int64                 `json:"size"`
	Downloaded      int64                 `json:"downloaded"`
	Progress        float64               `json:"progress"`
	// Persisted observations never grant file readiness. LegacyDownloaded is
	// the pre-upgrade observation, NOT a cumulative network transfer counter.
	ProgressSource     string            `json:"progress_source"`
	ProgressObservedAt int64             `json:"progress_observed_at_ns,omitempty"`
	LegacyDownloaded   int64             `json:"legacy_downloaded,omitempty"`
	MetadataKnown      bool              `json:"metadata_known"`
	CatalogVersion     string            `json:"catalog_version,omitempty"`
	SnapshotFreshness  string            `json:"snapshot_freshness,omitempty"`
	MetadataFormat     string            `json:"metadata_format,omitempty"`
	QueuePosition      int64             `json:"-"`
	SelectionComplete  bool              `json:"selection_complete"`
	Recheck            *RecheckOperation `json:"recheck,omitempty"`
	Activity           TorrentActivity   `json:"activity"`
	SnapshotAt         int64             `json:"snapshot_at_ns,omitempty"`
	State              TorrentState      `json:"state"`
	Error              ErrorReason       `json:"error,omitempty"`
	Files              []*File           `json:"files,omitempty"`
	// Piece info for UI progress
	PieceLength int64 `json:"piece_length"`
	NumPieces   int   `json:"num_pieces"`
	// Runtime statistics
	DownloadSpeed int64       `json:"download_speed"`
	UploadSpeed   int64       `json:"upload_speed"`
	Peers         int         `json:"peers"`
	Seeds         int         `json:"seeds"`
	PeerSummary   PeerSummary `json:"peer_summary"`

	SourceURI string `json:"-"`
}

// ResumeProgress is attachment-local cached diagnosis, never durable authority.
type ResumeProgress struct {
	Mode                 string `json:"mode"`
	Status               string `json:"status"`
	Reason               string `json:"reason"`
	CheckpointGeneration string `json:"checkpoint_generation,omitempty"`
	CheckpointAtMS       int64  `json:"checkpoint_at_ms,omitempty"`
	RestoredBytes        int64  `json:"restored_bytes"`
	FreshlyVerifiedBytes int64  `json:"freshly_verified_bytes"`
}

type RecheckOperation struct {
	ID     uint64 `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// PeerSummary contains runtime-only aggregate swarm diagnostics.
type PeerSummary struct {
	Known         int    `json:"known"`
	Connected     int    `json:"connected"`
	Pending       int    `json:"pending"`
	HalfOpen      int    `json:"half_open"`
	Seeds         int    `json:"seeds"`
	MetadataReady bool   `json:"metadata_ready"`
	TrackerStatus string `json:"tracker_status,omitempty"`
	TrackerError  string `json:"tracker_error,omitempty"`
	DHTStatus     string `json:"dht_status,omitempty"`
}

type BTHealth struct {
	SeedEnabled                bool              `json:"seed_enabled"`
	UploadEnabled              bool              `json:"upload_enabled"`
	DHTEnabled                 bool              `json:"dht_enabled"`
	PEXEnabled                 bool              `json:"pex_enabled"`
	UPNPEnabled                bool              `json:"upnp_enabled"`
	TCPEnabled                 bool              `json:"tcp_enabled"`
	UTPEnabled                 bool              `json:"utp_enabled"`
	IPv6Enabled                bool              `json:"ipv6_enabled"`
	ListenPort                 int               `json:"listen_port"`
	ClientProfile              string            `json:"client_profile"`
	DownloadProfile            string            `json:"download_profile"`
	EstablishedConnsPerTorrent int               `json:"established_conns_per_torrent"`
	HalfOpenConnsPerTorrent    int               `json:"half_open_conns_per_torrent"`
	TotalHalfOpenConns         int               `json:"total_half_open_conns"`
	PeersLowWater              int               `json:"peers_low_water"`
	PeersHighWater             int               `json:"peers_high_water"`
	DialRateLimit              int               `json:"dial_rate_limit"`
	PublicIPDiscoveryEnabled   bool              `json:"public_ip_discovery_enabled"`
	PublicIPv4Status           string            `json:"public_ipv4_status"`
	PublicIPv6Status           string            `json:"public_ipv6_status"`
	RetrackersMode             string            `json:"retrackers_mode"`
	DownloadLimit              int               `json:"download_limit"`
	UploadLimit                int               `json:"upload_limit"`
	SwarmWatchdogEnabled       bool              `json:"swarm_watchdog_enabled"`
	SwarmCheckIntervalSec      int               `json:"swarm_check_interval_sec"`
	SwarmRefreshCooldownSec    int               `json:"swarm_refresh_cooldown_sec"`
	InvalidMetainfoCount       int               `json:"invalid_metainfo_count"`
	PieceCompletionBackend     string            `json:"piece_completion_backend"`
	PieceCompletionPersistent  bool              `json:"piece_completion_persistent"`
	PieceCompletionError       string            `json:"piece_completion_error,omitempty"`
	PieceCompletionDropped     uint64            `json:"piece_completion_dropped"`
	PieceCompletionPending     int               `json:"piece_completion_pending"`
	PeerDropRatio              float64           `json:"peer_drop_ratio"`
	SeedDropRatio              float64           `json:"seed_drop_ratio"`
	SpeedDropRatio             float64           `json:"speed_drop_ratio"`
	IncomingConnectivityNote   string            `json:"incoming_connectivity_note"`
	Torrents                   []BTTorrentHealth `json:"torrents"`
}

type BTTorrentHealth struct {
	StorageIO             *StorageIOTimings `json:"storage_io,omitempty"`
	Hash                  string            `json:"hash"`
	Name                  string            `json:"name"`
	State                 TorrentState      `json:"state"`
	Known                 int               `json:"known"`
	Connected             int               `json:"connected"`
	Pending               int               `json:"pending"`
	HalfOpen              int               `json:"half_open"`
	Seeds                 int               `json:"seeds"`
	BytesRead             int64             `json:"bytes_read"`
	BytesReadData         int64             `json:"bytes_read_data"`
	BytesReadUsefulData   int64             `json:"bytes_read_useful_data"`
	BytesWritten          int64             `json:"bytes_written"`
	BytesWrittenData      int64             `json:"bytes_written_data"`
	ChunksRead            int64             `json:"chunks_read"`
	ChunksReadUseful      int64             `json:"chunks_read_useful"`
	ChunksReadWasted      int64             `json:"chunks_read_wasted"`
	PiecesDirtiedGood     int64             `json:"pieces_dirtied_good"`
	PiecesDirtiedBad      int64             `json:"pieces_dirtied_bad"`
	WasteRatio            float64           `json:"waste_ratio"`
	TrackerTiersCount     int               `json:"tracker_tiers_count"`
	TrackerURLsCount      int               `json:"tracker_urls_count"`
	MetadataReady         bool              `json:"metadata_ready"`
	TrackerStatus         string            `json:"tracker_status,omitempty"`
	TrackerError          string            `json:"tracker_error,omitempty"`
	DownloadSpeed         int64             `json:"download_speed"`
	UploadSpeed           int64             `json:"upload_speed"`
	Degraded              bool              `json:"degraded"`
	PeakDownloadSpeed     int64             `json:"peak_download_speed"`
	PeakUpdatedAt         string            `json:"peak_updated_at,omitempty"`
	LastRefreshAt         string            `json:"last_refresh_at,omitempty"`
	LastRefreshReason     string            `json:"last_refresh_reason,omitempty"`
	LastPeerRefreshAt     string            `json:"last_peer_refresh_at,omitempty"`
	LastPeerRefreshReason string            `json:"last_peer_refresh_reason,omitempty"`
	LastHealthyAt         string            `json:"last_healthy_at,omitempty"`
	BoostedUntil          string            `json:"boosted_until,omitempty"`
	MaxEstablishedConns   int               `json:"max_established_conns"`
	ActiveStreams         int               `json:"active_streams"`
}

// Attachment-local cumulative counters; nanoseconds include concurrent work.
type StorageIOTimings struct {
	ManagedPayload       bool                  `json:"managed_payload"`
	FileCacheHits        int64                 `json:"file_cache_hits"`
	FileCacheMisses      int64                 `json:"file_cache_misses"`
	FileCacheEvictions   int64                 `json:"file_cache_evictions"`
	FileCacheOpen        int64                 `json:"file_cache_open"`
	CompletionSyncCount  int64                 `json:"completion_sync_count"`
	CompletionSyncErrors int64                 `json:"completion_sync_errors"`
	CompletionSyncActive int64                 `json:"completion_sync_active"`
	CompletionSyncNS     int64                 `json:"completion_sync_ns"`
	CompletionSyncMaxNS  int64                 `json:"completion_sync_max_ns"`
	WriteLeaseWaitCount  int64                 `json:"write_lease_wait_count"`
	WriteLeaseWaitActive int64                 `json:"write_lease_wait_active"`
	WriteLeaseWaitNS     int64                 `json:"write_lease_wait_ns"`
	WriteLeaseWaitMaxNS  int64                 `json:"write_lease_wait_max_ns"`
	WriteLeaseHold       StorageIOStageTimings `json:"write_lease_hold"`
	WritePrepare         StorageIOStageTimings `json:"write_prepare"`
	WriteGuard           StorageIOStageTimings `json:"write_guard"`
	WriteOpen            StorageIOStageTimings `json:"write_open"`
	WriteStat            StorageIOStageTimings `json:"write_stat"`
	WritePayload         StorageIOStageTimings `json:"write_payload"`
	WriteAccount         StorageIOStageTimings `json:"write_account"`
	WriteClose           StorageIOStageTimings `json:"write_close"`
	WriteOpenGuard       StorageIOStageTimings `json:"write_open_guard"`
	WriteOpenPath        StorageIOStageTimings `json:"write_open_path"`
	WriteOpenMkdir       StorageIOStageTimings `json:"write_open_mkdir"`
	WriteOpenDescriptor  StorageIOStageTimings `json:"write_open_descriptor"`
	WriteOpenIdentity    StorageIOStageTimings `json:"write_open_identity"`
}

type StorageIOStageTimings struct {
	Count  int64 `json:"count"`
	Errors int64 `json:"errors"`
	Active int64 `json:"active"`
	NS     int64 `json:"ns"`
	MaxNS  int64 `json:"max_ns"`
}

// File represents a single file within a torrent
type File struct {
	MetadataPath     string       `json:"metadata_path,omitempty"`
	Status           *FileStatus  `json:"status,omitempty"`
	Index            int          `json:"index"`
	Path             string       `json:"path"`
	Size             int64        `json:"size"`
	Offset           int64        `json:"offset"`
	OffsetKnown      bool         `json:"offset_known"`
	LegacyDownloaded int64        `json:"legacy_downloaded,omitempty"`
	Downloaded       int64        `json:"downloaded"`
	Priority         FilePriority `json:"priority"`
	IsMedia          bool         `json:"is_media"` // Helper flag for UI (can be streamed)
}

// ProgressPercent is shared by runtime and persisted views. Unknown/empty size
// has no meaningful percentage; bytes alone never establish readiness.
func ProgressPercent(downloaded, size int64) float64 {
	if size <= 0 {
		return 0
	}
	return float64(max(int64(0), min(downloaded, size))) / float64(size) * 100
}
