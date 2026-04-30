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
	Size       int64        `json:"size"`
	Downloaded int64        `json:"downloaded"`
	Progress   float64      `json:"progress"`
	State      TorrentState `json:"state"`
	Error      ErrorReason  `json:"error,omitempty"`
	Files      []*File      `json:"files,omitempty"`
	// Runtime statistics
	DownloadSpeed int64 `json:"download_speed"`
	UploadSpeed   int64 `json:"upload_speed"`
	Peers         int   `json:"peers"`
	Seeds         int   `json:"seeds"`

	SourceURI string `json:"-"`
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
