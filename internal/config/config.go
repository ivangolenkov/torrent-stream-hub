package config

import (
	"flag"
	"os"
	"strconv"
)

type Config struct {
	Port               string
	TorrentPort        int
	DownloadDir        string
	DBPath             string
	MaxActiveStreams   int
	MaxActiveDownloads int
	MinFreeSpaceGB     int
	DownloadLimit      int
	UploadLimit        int
	StreamCacheSize    int64
	AuthEnabled        bool
	AuthUser           string
	AuthPassword       string
	LogLevel           string
	BTSeed             bool
	BTSeedConfigured   bool
	BTNoUpload         bool
	BTClientProfile    string
	BTRetrackersMode   string
	BTRetrackersFile   string
	BTDisableDHT       bool
	BTDisablePEX       bool
	BTDisableUPNP      bool
	BTDisableTCP       bool
	BTDisableUTP       bool
	BTDisableIPv6      bool
	BTEstablishedConns int
	BTHalfOpenConns    int
	BTTotalHalfOpen    int
	BTPeersLowWater    int
	BTPeersHighWater   int
	BTDialRateLimit    int
}

func Load() *Config {
	cfg := &Config{}

	flag.StringVar(&cfg.Port, "port", getEnv("HUB_PORT", "8080"), "Server port")
	flag.IntVar(&cfg.TorrentPort, "torrent-port", getEnvAsInt("HUB_TORRENT_PORT", 50007), "BitTorrent protocol port")
	flag.StringVar(&cfg.DownloadDir, "download-dir", getEnv("HUB_DOWNLOAD_DIR", "/downloads"), "Directory to save downloaded files")
	flag.StringVar(&cfg.DBPath, "db-path", getEnv("HUB_DB_PATH", "/config/hub.db"), "Path to SQLite database file")
	flag.IntVar(&cfg.MaxActiveStreams, "max-streams", getEnvAsInt("HUB_MAX_ACTIVE_STREAMS", 4), "Maximum number of active streams")
	flag.IntVar(&cfg.MaxActiveDownloads, "max-downloads", getEnvAsInt("HUB_MAX_ACTIVE_DOWNLOADS", 5), "Maximum number of active background downloads")
	flag.IntVar(&cfg.MinFreeSpaceGB, "min-free-space", getEnvAsInt("HUB_MIN_FREE_SPACE_GB", 5), "Minimum free space in GB to allow downloading")
	flag.IntVar(&cfg.DownloadLimit, "download-limit", getEnvAsInt("HUB_DOWNLOAD_LIMIT", 0), "Download speed limit in bytes/sec (0 = unlimited)")
	flag.IntVar(&cfg.UploadLimit, "upload-limit", getEnvAsInt("HUB_UPLOAD_LIMIT", 0), "Upload speed limit in bytes/sec (0 = unlimited)")
	flag.Int64Var(&cfg.StreamCacheSize, "stream-cache-size", getEnvAsInt64("HUB_STREAM_CACHE_SIZE", 209715200), "Stream sliding window cache size in bytes")
	flag.BoolVar(&cfg.AuthEnabled, "auth-enabled", getEnvAsBool("HUB_AUTH_ENABLED", false), "Enable basic authentication")
	flag.StringVar(&cfg.AuthUser, "auth-user", getEnv("HUB_AUTH_USER", "admin"), "Basic auth username")
	flag.StringVar(&cfg.AuthPassword, "auth-password", getEnv("HUB_AUTH_PASSWORD", "admin"), "Basic auth password")
	flag.BoolVar(&cfg.BTSeed, "bt-seed", getEnvAsBool("HUB_BT_SEED", true), "Enable altruistic seeding after download completion")
	flag.BoolVar(&cfg.BTNoUpload, "bt-no-upload", getEnvAsBool("HUB_BT_NO_UPLOAD", false), "Disable BitTorrent uploads")
	flag.StringVar(&cfg.BTClientProfile, "bt-client-profile", getEnv("HUB_BT_CLIENT_PROFILE", "qbittorrent"), "BitTorrent client profile: qbittorrent or default")
	flag.StringVar(&cfg.BTRetrackersMode, "bt-retrackers-mode", getEnv("HUB_BT_RETRACKERS_MODE", "append"), "Retrackers mode: append, replace, off")
	flag.StringVar(&cfg.BTRetrackersFile, "bt-retrackers-file", getEnv("HUB_BT_RETRACKERS_FILE", "/config/trackers.txt"), "Path to additional retrackers list")
	flag.BoolVar(&cfg.BTDisableDHT, "bt-disable-dht", getEnvAsBool("HUB_BT_DISABLE_DHT", false), "Disable BitTorrent DHT")
	flag.BoolVar(&cfg.BTDisablePEX, "bt-disable-pex", getEnvAsBool("HUB_BT_DISABLE_PEX", false), "Disable BitTorrent PEX")
	flag.BoolVar(&cfg.BTDisableUPNP, "bt-disable-upnp", getEnvAsBool("HUB_BT_DISABLE_UPNP", false), "Disable UPnP port forwarding")
	flag.BoolVar(&cfg.BTDisableTCP, "bt-disable-tcp", getEnvAsBool("HUB_BT_DISABLE_TCP", false), "Disable BitTorrent TCP")
	flag.BoolVar(&cfg.BTDisableUTP, "bt-disable-utp", getEnvAsBool("HUB_BT_DISABLE_UTP", false), "Disable BitTorrent uTP")
	flag.BoolVar(&cfg.BTDisableIPv6, "bt-disable-ipv6", getEnvAsBool("HUB_BT_DISABLE_IPV6", false), "Disable BitTorrent IPv6")
	flag.IntVar(&cfg.BTEstablishedConns, "bt-established-conns", getEnvAsInt("HUB_BT_ESTABLISHED_CONNS_PER_TORRENT", 50), "Established peer connections per torrent")
	flag.IntVar(&cfg.BTHalfOpenConns, "bt-half-open-conns", getEnvAsInt("HUB_BT_HALF_OPEN_CONNS_PER_TORRENT", 50), "Half-open peer connections per torrent")
	flag.IntVar(&cfg.BTTotalHalfOpen, "bt-total-half-open", getEnvAsInt("HUB_BT_TOTAL_HALF_OPEN_CONNS", 500), "Total half-open peer connections")
	flag.IntVar(&cfg.BTPeersLowWater, "bt-peers-low-water", getEnvAsInt("HUB_BT_PEERS_LOW_WATER", 100), "Minimum peer reserve before more discovery")
	flag.IntVar(&cfg.BTPeersHighWater, "bt-peers-high-water", getEnvAsInt("HUB_BT_PEERS_HIGH_WATER", 1000), "Maximum peer reserve")
	flag.IntVar(&cfg.BTDialRateLimit, "bt-dial-rate-limit", getEnvAsInt("HUB_BT_DIAL_RATE_LIMIT", 20), "Peer dial rate limit per second")

	flag.Parse()
	cfg.BTSeedConfigured = true
	cfg.LogLevel = getEnv("HUB_LOG_LEVEL", "debug")
	ApplyDefaults(cfg)

	return cfg
}

func ApplyDefaults(cfg *Config) {
	if cfg == nil {
		return
	}
	if !cfg.BTSeedConfigured {
		cfg.BTSeed = true
	}
	if cfg.BTClientProfile == "" {
		cfg.BTClientProfile = "qbittorrent"
	}
	if cfg.BTRetrackersMode == "" {
		cfg.BTRetrackersMode = "append"
	}
	if cfg.BTRetrackersFile == "" {
		cfg.BTRetrackersFile = "/config/trackers.txt"
	}
	if cfg.BTEstablishedConns <= 0 {
		cfg.BTEstablishedConns = 50
	}
	if cfg.BTHalfOpenConns <= 0 {
		cfg.BTHalfOpenConns = 50
	}
	if cfg.BTTotalHalfOpen <= 0 {
		cfg.BTTotalHalfOpen = 500
	}
	if cfg.BTPeersLowWater <= 0 {
		cfg.BTPeersLowWater = 100
	}
	if cfg.BTPeersHighWater <= 0 {
		cfg.BTPeersHighWater = 1000
	}
	if cfg.BTDialRateLimit <= 0 {
		cfg.BTDialRateLimit = 20
	}
	if cfg.BTPeersHighWater < cfg.BTPeersLowWater {
		cfg.BTPeersHighWater = cfg.BTPeersLowWater
	}
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func getEnvAsInt(key string, fallback int) int {
	if valueStr, exists := os.LookupEnv(key); exists {
		if value, err := strconv.Atoi(valueStr); err == nil {
			return value
		}
	}
	return fallback
}

func getEnvAsInt64(key string, fallback int64) int64 {
	if valueStr, exists := os.LookupEnv(key); exists {
		if value, err := strconv.ParseInt(valueStr, 10, 64); err == nil {
			return value
		}
	}
	return fallback
}

func getEnvAsBool(key string, fallback bool) bool {
	if valueStr, exists := os.LookupEnv(key); exists {
		if value, err := strconv.ParseBool(valueStr); err == nil {
			return value
		}
	}
	return fallback
}
