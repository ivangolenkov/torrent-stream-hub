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

// File represents a single file within a torrent
type File struct {
	Index      int          `json:"index"`
	Path       string       `json:"path"`
	Size       int64        `json:"size"`
	Downloaded int64        `json:"downloaded"`
	Priority   FilePriority `json:"priority"`
	IsMedia    bool         `json:"is_media"` // Helper flag for UI (can be streamed)
}
