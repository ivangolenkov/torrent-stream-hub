package models

// TorrentState represents the strictly defined finite state machine of a torrent
type TorrentState string

const (
	StateQueued       TorrentState = "Queued"
	StateDownloading  TorrentState = "Downloading"
	StateStreaming    TorrentState = "Streaming"
	StateSeeding      TorrentState = "Seeding"
	StatePaused       TorrentState = "Paused"
	StateError        TorrentState = "Error"
	StateMissingFiles TorrentState = "MissingFiles"
	StateDiskFull     TorrentState = "DiskFull"
)

// ErrorReason provides a detailed cause when a torrent enters the Error or DiskFull state
type ErrorReason string

const (
	ErrInvalidTorrent     ErrorReason = "Invalid torrent"
	ErrTrackerUnreachable ErrorReason = "Tracker unreachable"
	ErrNoPeers            ErrorReason = "No peers"
	ErrDiskFull           ErrorReason = "Disk full"
	ErrMissingFiles       ErrorReason = "Missing files"
	ErrNone               ErrorReason = ""
)

// FilePriority dictates the download priority for specific files in a torrent
type FilePriority int

const (
	PriorityNormal FilePriority = 0
	PriorityHigh   FilePriority = 1
	PriorityNone   FilePriority = -1
)

// Torrent defines the core properties and state of a single torrent
type Torrent struct {
	Hash       string       `json:"hash"`
	Name       string       `json:"name"`
	Title      string       `json:"title,omitempty"`
	Data       string       `json:"data,omitempty"`
	Poster     string       `json:"poster,omitempty"`
	Category   string       `json:"category,omitempty"`
	Size       int64        `json:"size"`
	Downloaded int64        `json:"downloaded"`
	Progress   float64      `json:"progress"`
	State      TorrentState `json:"state"`
	Error      ErrorReason  `json:"error,omitempty"`
	Files      []*File      `json:"files,omitempty"`
	// Runtime statistics
	DownloadSpeed int64       `json:"download_speed"`
	UploadSpeed   int64       `json:"upload_speed"`
	Peers         int         `json:"peers"`
	Seeds         int         `json:"seeds"`
	PeerSummary   PeerSummary `json:"peer_summary"`

	SourceURI string `json:"-"`
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
	SeedEnabled                 bool              `json:"seed_enabled"`
	UploadEnabled               bool              `json:"upload_enabled"`
	DHTEnabled                  bool              `json:"dht_enabled"`
	PEXEnabled                  bool              `json:"pex_enabled"`
	UPNPEnabled                 bool              `json:"upnp_enabled"`
	TCPEnabled                  bool              `json:"tcp_enabled"`
	UTPEnabled                  bool              `json:"utp_enabled"`
	IPv6Enabled                 bool              `json:"ipv6_enabled"`
	ListenPort                  int               `json:"listen_port"`
	ClientProfile               string            `json:"client_profile"`
	RetrackersMode              string            `json:"retrackers_mode"`
	DownloadLimit               int               `json:"download_limit"`
	UploadLimit                 int               `json:"upload_limit"`
	SwarmWatchdogEnabled        bool              `json:"swarm_watchdog_enabled"`
	SwarmCheckIntervalSec       int               `json:"swarm_check_interval_sec"`
	SwarmRefreshCooldownSec     int               `json:"swarm_refresh_cooldown_sec"`
	HardRefreshEnabled          bool              `json:"hard_refresh_enabled"`
	HardRefreshCooldownSec      int               `json:"hard_refresh_cooldown_sec"`
	HardRefreshAfterSoftFails   int               `json:"hard_refresh_after_soft_fails"`
	ClientRecycleEnabled        bool              `json:"client_recycle_enabled"`
	ClientRecycleCooldownSec    int               `json:"client_recycle_cooldown_sec"`
	ClientRecycleAfterHardFails int               `json:"client_recycle_after_hard_fails"`
	ClientRecycleCount          int               `json:"client_recycle_count"`
	ClientRecycleCountLastHour  int               `json:"client_recycle_count_last_hour"`
	LastClientRecycleAt         string            `json:"last_client_recycle_at,omitempty"`
	LastClientRecycleReason     string            `json:"last_client_recycle_reason,omitempty"`
	LastClientRecycleError      string            `json:"last_client_recycle_error,omitempty"`
	ClientRecycleAllowed        bool              `json:"client_recycle_allowed"`
	ClientRecycleBlockedReason  string            `json:"client_recycle_blocked_reason,omitempty"`
	NextClientRecycleAt         string            `json:"next_client_recycle_at,omitempty"`
	PeerDropRatio               float64           `json:"peer_drop_ratio"`
	SeedDropRatio               float64           `json:"seed_drop_ratio"`
	SpeedDropRatio              float64           `json:"speed_drop_ratio"`
	IncomingConnectivityNote    string            `json:"incoming_connectivity_note"`
	Torrents                    []BTTorrentHealth `json:"torrents"`
}

type BTTorrentHealth struct {
	Hash                            string       `json:"hash"`
	Name                            string       `json:"name"`
	State                           TorrentState `json:"state"`
	Known                           int          `json:"known"`
	Connected                       int          `json:"connected"`
	Pending                         int          `json:"pending"`
	HalfOpen                        int          `json:"half_open"`
	Seeds                           int          `json:"seeds"`
	TrackerStatus                   string       `json:"tracker_status,omitempty"`
	TrackerError                    string       `json:"tracker_error,omitempty"`
	DownloadSpeed                   int64        `json:"download_speed"`
	UploadSpeed                     int64        `json:"upload_speed"`
	Degraded                        bool         `json:"degraded"`
	LastRefreshAt                   string       `json:"last_refresh_at,omitempty"`
	LastRefreshReason               string       `json:"last_refresh_reason,omitempty"`
	LastHealthyAt                   string       `json:"last_healthy_at,omitempty"`
	BoostedUntil                    string       `json:"boosted_until,omitempty"`
	MaxEstablishedConns             int          `json:"max_established_conns"`
	PeakConnected                   int          `json:"peak_connected"`
	PeakSeeds                       int          `json:"peak_seeds"`
	PeakDownloadSpeed               int64        `json:"peak_download_speed"`
	PeakUpdatedAt                   string       `json:"peak_updated_at,omitempty"`
	SoftRefreshCount                int          `json:"soft_refresh_count"`
	HardRefreshCount                int          `json:"hard_refresh_count"`
	LastHardRefreshAt               string       `json:"last_hard_refresh_at,omitempty"`
	LastHardRefreshReason           string       `json:"last_hard_refresh_reason,omitempty"`
	LastHardRefreshError            string       `json:"last_hard_refresh_error,omitempty"`
	HardRefreshAllowed              bool         `json:"hard_refresh_allowed"`
	HardRefreshBlockedReason        string       `json:"hard_refresh_blocked_reason,omitempty"`
	ActiveStreams                   int          `json:"active_streams"`
	DegradationEpisodeStartedAt     string       `json:"degradation_episode_started_at,omitempty"`
	LastDegradedAt                  string       `json:"last_degraded_at,omitempty"`
	LastRecoveredAt                 string       `json:"last_recovered_at,omitempty"`
	LastSoftRefreshAt               string       `json:"last_soft_refresh_at,omitempty"`
	LastSoftRefreshReason           string       `json:"last_soft_refresh_reason,omitempty"`
	SoftRefreshAttemptsInEpisode    int          `json:"soft_refresh_attempts_in_episode"`
	HardRefreshAttemptsInEpisode    int          `json:"hard_refresh_attempts_in_episode"`
	LastSoftRefreshCountResetReason string       `json:"last_soft_refresh_count_reset_reason,omitempty"`
	NextHardRefreshAt               string       `json:"next_hard_refresh_at,omitempty"`
	NextClientRecycleAt             string       `json:"next_client_recycle_at,omitempty"`
}

// File represents a single file within a torrent
type File struct {
	Index      int          `json:"index"`
	Path       string       `json:"path"`
	Size       int64        `json:"size"`
	Downloaded int64        `json:"downloaded"`
	Priority   FilePriority `json:"priority"`
	IsMedia    bool         `json:"is_media"` // Helper flag for UI (can be streamed)
}
