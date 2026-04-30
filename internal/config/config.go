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

	flag.Parse()
	cfg.LogLevel = getEnv("HUB_LOG_LEVEL", "debug")

	return cfg
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
