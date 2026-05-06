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
	BenchmarkMode              bool              `json:"benchmark_mode"`
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
	PeerDropRatio              float64           `json:"peer_drop_ratio"`
	SeedDropRatio              float64           `json:"seed_drop_ratio"`
	SpeedDropRatio             float64           `json:"speed_drop_ratio"`
	IncomingConnectivityNote   string            `json:"incoming_connectivity_note"`
	Torrents                   []BTTorrentHealth `json:"torrents"`
}

type BTTorrentHealth struct {
	Hash                  string       `json:"hash"`
	Name                  string       `json:"name"`
	State                 TorrentState `json:"state"`
	Known                 int          `json:"known"`
	Connected             int          `json:"connected"`
	Pending               int          `json:"pending"`
	HalfOpen              int          `json:"half_open"`
	Seeds                 int          `json:"seeds"`
	BytesRead             int64        `json:"bytes_read"`
	BytesReadData         int64        `json:"bytes_read_data"`
	BytesReadUsefulData   int64        `json:"bytes_read_useful_data"`
	BytesWritten          int64        `json:"bytes_written"`
	BytesWrittenData      int64        `json:"bytes_written_data"`
	ChunksRead            int64        `json:"chunks_read"`
	ChunksReadUseful      int64        `json:"chunks_read_useful"`
	ChunksReadWasted      int64        `json:"chunks_read_wasted"`
	PiecesDirtiedGood     int64        `json:"pieces_dirtied_good"`
	PiecesDirtiedBad      int64        `json:"pieces_dirtied_bad"`
	WasteRatio            float64      `json:"waste_ratio"`
	TrackerTiersCount     int          `json:"tracker_tiers_count"`
	TrackerURLsCount      int          `json:"tracker_urls_count"`
	MetadataReady         bool         `json:"metadata_ready"`
	TrackerStatus         string       `json:"tracker_status,omitempty"`
	TrackerError          string       `json:"tracker_error,omitempty"`
	DownloadSpeed         int64        `json:"download_speed"`
	UploadSpeed           int64        `json:"upload_speed"`
	Degraded              bool         `json:"degraded"`
	LastRefreshAt         string       `json:"last_refresh_at,omitempty"`
	LastRefreshReason     string       `json:"last_refresh_reason,omitempty"`
	LastPeerRefreshAt     string       `json:"last_peer_refresh_at,omitempty"`
	LastPeerRefreshReason string       `json:"last_peer_refresh_reason,omitempty"`
	LastHealthyAt         string       `json:"last_healthy_at,omitempty"`
	BoostedUntil          string       `json:"boosted_until,omitempty"`
	MaxEstablishedConns   int          `json:"max_established_conns"`
	ActiveStreams         int          `json:"active_streams"`
}

// File represents a single file within a torrent
type File struct {
	Index      int          `json:"index"`
	Path       string       `json:"path"`
	Size       int64        `json:"size"`
	Offset     int64        `json:"offset"`
	Downloaded int64        `json:"downloaded"`
	Priority   FilePriority `json:"priority"`
	IsMedia    bool         `json:"is_media"` // Helper flag for UI (can be streamed)
}
