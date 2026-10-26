package config

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"torrent-stream-hub/internal/metafetch"
)

type Config struct {
	FastResumeMode             string
	fastResumeConfigured       bool
	DeprecatedOptions          []string
	MetainfoTempDir            string
	MetainfoAllowlist          string
	MaxTemporaryTorrents       int
	temporaryConfigError       error
	Port                       string
	TorrentPort                int
	DownloadDir                string
	DBPath                     string
	MaxActiveStreams           int
	MaxStreamsConfigured       bool
	MaxActiveDownloads         int
	MinFreeSpaceGB             int
	DownloadLimit              int
	UploadLimit                int
	StreamCacheSize            int64
	LogLevel                   string
	LogDir                     string
	LogRetentionDays           int
	LogMaxFileBytes            int64
	LogMaxTotalBytes           int64
	logConfigError             error
	BTSeed                     bool
	BTSeedConfigured           bool
	BTNoUpload                 bool
	BTClientProfile            string
	BTDownloadProfile          string
	BTBenchmarkMode            bool
	BTPublicIPDiscoveryEnabled bool
	BTPublicIPv4               string
	BTPublicIPv6               string
	BTPublicIPv4Status         string
	BTPublicIPv6Status         string
	BTRetrackersMode           string
	BTRetrackersFile           string
	BTDisableDHT               bool
	BTDisablePEX               bool
	BTDisableUPNP              bool
	BTDisableTCP               bool
	BTDisableUTP               bool
	BTDisableIPv6              bool
	BTEstablishedConns         int
	BTHalfOpenConns            int
	BTTotalHalfOpen            int
	BTPeersLowWater            int
	BTPeersHighWater           int
	BTDialRateLimit            int
	BTSwarmWatchdogEnabled     bool
	BTSwarmWatchdogConfigured  bool
	BTSwarmCheckIntervalSec    int
	BTSwarmRefreshCooldownSec  int
	BTSwarmMinConnectedPeers   int
	BTSwarmMinConnectedSeeds   int
	BTSwarmStalledSpeedBps     int
	BTSwarmStalledDurationSec  int
	BTSwarmBoostConns          int
	BTSwarmBoostDurationSec    int
	BTSwarmPeerDropRatio       float64
	BTSwarmSeedDropRatio       float64
	BTSwarmSpeedDropRatio      float64
	BTSwarmPeakTTLSec          int
	// Deprecated compatibility fields below have no runtime effect.
	BTSwarmHardRefreshEnabled          bool
	BTSwarmHardRefreshConfigured       bool
	BTSwarmAutoHardRefreshEnabled      bool
	BTSwarmHardRefreshCooldownSec      int
	BTSwarmHardRefreshAfterSoftFails   int
	BTSwarmHardRefreshMinTorrentAgeSec int
	BTSwarmDegradationEpisodeTTLSec    int
	BTSwarmRecoveryGraceSec            int
	BTClientRecycleEnabled             bool
	BTClientRecycleConfigured          bool
	BTClientRecycleCooldownSec         int
	BTClientRecycleAfterHardFails      int
	BTClientRecycleAfterSoftFails      int
	BTClientRecycleMinTorrentAgeSec    int
	BTClientRecycleMinTorrents         int
	BTClientRecycleMaxPerHour          int
}

func Load() *Config {
	cfg := &Config{}
	var logRetention string
	var maxTemporary string
	var logMaxFile, logMaxTotal string

	flag.StringVar(&cfg.Port, "port", getEnv("HUB_PORT", "8080"), "Server port")
	flag.IntVar(&cfg.TorrentPort, "torrent-port", getEnvAsInt("HUB_TORRENT_PORT", 50007), "BitTorrent protocol port")
	flag.StringVar(&cfg.DownloadDir, "download-dir", getEnv("HUB_DOWNLOAD_DIR", "/downloads"), "Directory to save downloaded files")
	flag.StringVar(&cfg.DBPath, "db-path", getEnv("HUB_DB_PATH", "/config/hub.db"), "Path to SQLite database file")
	flag.StringVar(&cfg.FastResumeMode, "fast-resume-mode", getEnv("HUB_FAST_RESUME_MODE", "fast"), "Restart-scoped resume policy: fast (assumes no external payload writers) or verify (hash on every open)")
	flag.StringVar(&cfg.LogDir, "log-dir", getEnv("HUB_LOG_DIR", ""), "Log directory (default: logs beside database)")
	flag.StringVar(&logMaxFile, "log-max-file-bytes", getEnv("HUB_LOG_MAX_FILE_BYTES", "16777216"), "Maximum bytes per log segment")
	flag.StringVar(&logMaxTotal, "log-max-total-bytes", getEnv("HUB_LOG_MAX_TOTAL_BYTES", "134217728"), "Maximum managed log bytes")
	flag.StringVar(&logRetention, "log-retention-days", getEnv("HUB_LOG_RETENTION_DAYS", "3"), "Previous calendar days of logs to retain (0 = today only)")
	flag.StringVar(&maxTemporary, "max-temporary-torrents", getEnv("HUB_MAX_TEMPORARY_TORRENTS", "3"), "Maximum temporary torrents (positive integer)")
	flag.StringVar(&cfg.MetainfoTempDir, "metainfo-temp-dir", getEnv("HUB_METAINFO_TEMP_DIR", ""), "Managed metainfo upload directory")
	flag.StringVar(&cfg.MetainfoAllowlist, "metainfo-allowlist", getEnv("HUB_METAINFO_ALLOWLIST", ""), "Allowed LAN metainfo sources (comma-separated host:port or CIDR)")
	flag.IntVar(&cfg.MaxActiveStreams, "max-streams", getEnvAsInt("HUB_MAX_ACTIVE_STREAMS", 4), "Maximum number of active streams")
	flag.IntVar(&cfg.MaxActiveDownloads, "max-downloads", getEnvAsInt("HUB_MAX_ACTIVE_DOWNLOADS", 5), "Maximum number of active background downloads")
	flag.IntVar(&cfg.MinFreeSpaceGB, "min-free-space", getEnvAsInt("HUB_MIN_FREE_SPACE_GB", 5), "Minimum free space in GB to allow downloading")
	flag.IntVar(&cfg.DownloadLimit, "download-limit", getEnvAsInt("HUB_DOWNLOAD_LIMIT", 0), "Download speed limit in bytes/sec (0 = unlimited)")
	flag.IntVar(&cfg.UploadLimit, "upload-limit", getEnvAsInt("HUB_UPLOAD_LIMIT", 0), "Upload speed limit in bytes/sec (0 = unlimited)")
	flag.Int64Var(&cfg.StreamCacheSize, "stream-cache-size", getEnvAsInt64("HUB_STREAM_CACHE_SIZE", 209715200), "Stream sliding window cache size in bytes")
	flag.BoolVar(&cfg.BTSeed, "bt-seed", getEnvAsBool("HUB_BT_SEED", true), "Enable altruistic seeding after download completion")
	flag.BoolVar(&cfg.BTNoUpload, "bt-no-upload", getEnvAsBool("HUB_BT_NO_UPLOAD", false), "Disable BitTorrent uploads")
	flag.StringVar(&cfg.BTClientProfile, "bt-client-profile", getEnv("HUB_BT_CLIENT_PROFILE", "qbittorrent"), "BitTorrent client profile: qbittorrent or default")
	flag.StringVar(&cfg.BTDownloadProfile, "bt-download-profile", getEnv("HUB_BT_DOWNLOAD_PROFILE", "balanced"), "BitTorrent download profile: torrserver, balanced, aggressive")
	flag.BoolVar(&cfg.BTBenchmarkMode, "bt-benchmark-mode", getEnvAsBool("HUB_BT_BENCHMARK_MODE", false), "Deprecated: accepted for compatibility; has no effect")
	flag.BoolVar(&cfg.BTPublicIPDiscoveryEnabled, "bt-public-ip-discovery-enabled", getEnvAsBool("HUB_BT_PUBLIC_IP_DISCOVERY_ENABLED", false), "Enable best-effort public IP discovery for BitTorrent announces")
	flag.StringVar(&cfg.BTPublicIPv4, "bt-public-ipv4", getEnv("HUB_BT_PUBLIC_IPV4", ""), "Public IPv4 to advertise to BitTorrent peers")
	flag.StringVar(&cfg.BTPublicIPv6, "bt-public-ipv6", getEnv("HUB_BT_PUBLIC_IPV6", ""), "Public IPv6 to advertise to BitTorrent peers")
	flag.StringVar(&cfg.BTRetrackersMode, "bt-retrackers-mode", getEnv("HUB_BT_RETRACKERS_MODE", "append"), "Retrackers mode: append, replace, off")
	flag.StringVar(&cfg.BTRetrackersFile, "bt-retrackers-file", getEnv("HUB_BT_RETRACKERS_FILE", "/config/trackers.txt"), "Path to additional retrackers list")
	flag.BoolVar(&cfg.BTDisableDHT, "bt-disable-dht", getEnvAsBool("HUB_BT_DISABLE_DHT", false), "Disable BitTorrent DHT")
	flag.BoolVar(&cfg.BTDisablePEX, "bt-disable-pex", getEnvAsBool("HUB_BT_DISABLE_PEX", false), "Disable BitTorrent PEX")
	flag.BoolVar(&cfg.BTDisableUPNP, "bt-disable-upnp", getEnvAsBool("HUB_BT_DISABLE_UPNP", false), "Disable UPnP port forwarding")
	flag.BoolVar(&cfg.BTDisableTCP, "bt-disable-tcp", getEnvAsBool("HUB_BT_DISABLE_TCP", false), "Disable BitTorrent TCP")
	flag.BoolVar(&cfg.BTDisableUTP, "bt-disable-utp", getEnvAsBool("HUB_BT_DISABLE_UTP", true), "Disable BitTorrent uTP")
	flag.BoolVar(&cfg.BTDisableIPv6, "bt-disable-ipv6", getEnvAsBool("HUB_BT_DISABLE_IPV6", false), "Disable BitTorrent IPv6")
	flag.IntVar(&cfg.BTEstablishedConns, "bt-established-conns", getEnvAsInt("HUB_BT_ESTABLISHED_CONNS_PER_TORRENT", 0), "Established peer connections per torrent")
	flag.IntVar(&cfg.BTHalfOpenConns, "bt-half-open-conns", getEnvAsInt("HUB_BT_HALF_OPEN_CONNS_PER_TORRENT", 0), "Half-open peer connections per torrent")
	flag.IntVar(&cfg.BTTotalHalfOpen, "bt-total-half-open", getEnvAsInt("HUB_BT_TOTAL_HALF_OPEN_CONNS", 0), "Total half-open peer connections")
	flag.IntVar(&cfg.BTPeersLowWater, "bt-peers-low-water", getEnvAsInt("HUB_BT_PEERS_LOW_WATER", 0), "Minimum peer reserve before more discovery")
	flag.IntVar(&cfg.BTPeersHighWater, "bt-peers-high-water", getEnvAsInt("HUB_BT_PEERS_HIGH_WATER", 0), "Maximum peer reserve")
	flag.IntVar(&cfg.BTDialRateLimit, "bt-dial-rate-limit", getEnvAsInt("HUB_BT_DIAL_RATE_LIMIT", 0), "Peer dial rate limit per second")
	flag.BoolVar(&cfg.BTSwarmWatchdogEnabled, "bt-swarm-watchdog-enabled", getEnvAsBool("HUB_BT_SWARM_WATCHDOG_ENABLED", true), "Enable swarm health watchdog")
	flag.IntVar(&cfg.BTSwarmCheckIntervalSec, "bt-swarm-check-interval", getEnvAsInt("HUB_BT_SWARM_CHECK_INTERVAL_SEC", 60), "Swarm health check interval in seconds")
	flag.IntVar(&cfg.BTSwarmRefreshCooldownSec, "bt-swarm-refresh-cooldown", getEnvAsInt("HUB_BT_SWARM_REFRESH_COOLDOWN_SEC", 180), "Minimum seconds between swarm refresh actions per torrent")
	flag.IntVar(&cfg.BTSwarmMinConnectedPeers, "bt-swarm-min-connected-peers", getEnvAsInt("HUB_BT_SWARM_MIN_CONNECTED_PEERS", 8), "Minimum healthy connected peers")
	flag.IntVar(&cfg.BTSwarmMinConnectedSeeds, "bt-swarm-min-connected-seeds", getEnvAsInt("HUB_BT_SWARM_MIN_CONNECTED_SEEDS", 2), "Minimum healthy connected seeds for incomplete torrents")
	flag.IntVar(&cfg.BTSwarmStalledSpeedBps, "bt-swarm-stalled-speed", getEnvAsInt("HUB_BT_SWARM_STALLED_SPEED_BPS", 32768), "Download speed threshold for stalled torrent detection")
	flag.IntVar(&cfg.BTSwarmStalledDurationSec, "bt-swarm-stalled-duration", getEnvAsInt("HUB_BT_SWARM_STALLED_DURATION_SEC", 180), "Seconds below stalled speed before refresh")
	flag.IntVar(&cfg.BTSwarmBoostConns, "bt-swarm-boost-conns", getEnvAsInt("HUB_BT_SWARM_BOOST_CONNS", 120), "Temporary max established connections during swarm refresh")
	flag.IntVar(&cfg.BTSwarmBoostDurationSec, "bt-swarm-boost-duration", getEnvAsInt("HUB_BT_SWARM_BOOST_DURATION_SEC", 300), "Seconds to keep boosted max connections")
	flag.Float64Var(&cfg.BTSwarmPeerDropRatio, "bt-swarm-peer-drop-ratio", getEnvAsFloat("HUB_BT_SWARM_PEER_DROP_RATIO", 0.45), "Connected peer ratio below recent peak that marks a torrent degraded")
	flag.Float64Var(&cfg.BTSwarmSeedDropRatio, "bt-swarm-seed-drop-ratio", getEnvAsFloat("HUB_BT_SWARM_SEED_DROP_RATIO", 0.45), "Connected seed ratio below recent peak that marks an incomplete torrent degraded")
	flag.Float64Var(&cfg.BTSwarmSpeedDropRatio, "bt-swarm-speed-drop-ratio", getEnvAsFloat("HUB_BT_SWARM_SPEED_DROP_RATIO", 0.35), "Download speed ratio below recent peak that marks an incomplete torrent degraded")
	flag.IntVar(&cfg.BTSwarmPeakTTLSec, "bt-swarm-peak-ttl", getEnvAsInt("HUB_BT_SWARM_PEAK_TTL_SEC", 1800), "Seconds to keep swarm peak metrics for trend detection")
	flag.BoolVar(&cfg.BTSwarmHardRefreshEnabled, "bt-swarm-hard-refresh-enabled", getEnvAsBool("HUB_BT_SWARM_HARD_REFRESH_ENABLED", false), "Deprecated: accepted for compatibility; has no effect")
	flag.BoolVar(&cfg.BTSwarmAutoHardRefreshEnabled, "bt-swarm-auto-hard-refresh-enabled", getEnvAsBool("HUB_BT_SWARM_AUTO_HARD_REFRESH_ENABLED", false), "Deprecated: accepted for compatibility; has no effect")
	flag.IntVar(&cfg.BTSwarmHardRefreshCooldownSec, "bt-swarm-hard-refresh-cooldown", getEnvAsInt("HUB_BT_SWARM_HARD_REFRESH_COOLDOWN_SEC", 0), "Deprecated: accepted for compatibility; has no effect")
	flag.IntVar(&cfg.BTSwarmHardRefreshAfterSoftFails, "bt-swarm-hard-refresh-after-soft-fails", getEnvAsInt("HUB_BT_SWARM_HARD_REFRESH_AFTER_SOFT_FAILS", 0), "Deprecated: accepted for compatibility; has no effect")
	flag.IntVar(&cfg.BTSwarmHardRefreshMinTorrentAgeSec, "bt-swarm-hard-refresh-min-torrent-age", getEnvAsInt("HUB_BT_SWARM_HARD_REFRESH_MIN_TORRENT_AGE_SEC", 0), "Deprecated: accepted for compatibility; has no effect")
	flag.IntVar(&cfg.BTSwarmDegradationEpisodeTTLSec, "bt-swarm-degradation-episode-ttl", getEnvAsInt("HUB_BT_SWARM_DEGRADATION_EPISODE_TTL_SEC", 0), "Deprecated: accepted for compatibility; has no effect")
	flag.IntVar(&cfg.BTSwarmRecoveryGraceSec, "bt-swarm-recovery-grace", getEnvAsInt("HUB_BT_SWARM_RECOVERY_GRACE_SEC", 0), "Deprecated: accepted for compatibility; has no effect")
	flag.BoolVar(&cfg.BTClientRecycleEnabled, "bt-client-recycle-enabled", getEnvAsBool("HUB_BT_CLIENT_RECYCLE_ENABLED", false), "Deprecated: accepted for compatibility; has no effect")
	flag.IntVar(&cfg.BTClientRecycleCooldownSec, "bt-client-recycle-cooldown", getEnvAsInt("HUB_BT_CLIENT_RECYCLE_COOLDOWN_SEC", 0), "Deprecated: accepted for compatibility; has no effect")
	flag.IntVar(&cfg.BTClientRecycleAfterHardFails, "bt-client-recycle-after-hard-fails", getEnvAsInt("HUB_BT_CLIENT_RECYCLE_AFTER_HARD_FAILS", 0), "Deprecated: accepted for compatibility; has no effect")
	flag.IntVar(&cfg.BTClientRecycleAfterSoftFails, "bt-client-recycle-after-soft-fails", getEnvAsInt("HUB_BT_CLIENT_RECYCLE_AFTER_SOFT_FAILS", 0), "Deprecated: accepted for compatibility; has no effect")
	flag.IntVar(&cfg.BTClientRecycleMinTorrentAgeSec, "bt-client-recycle-min-torrent-age", getEnvAsInt("HUB_BT_CLIENT_RECYCLE_MIN_TORRENT_AGE_SEC", 0), "Deprecated: accepted for compatibility; has no effect")
	flag.IntVar(&cfg.BTClientRecycleMinTorrents, "bt-client-recycle-min-torrents", getEnvAsInt("HUB_BT_CLIENT_RECYCLE_MIN_TORRENTS", 0), "Deprecated: accepted for compatibility; has no effect")
	flag.IntVar(&cfg.BTClientRecycleMaxPerHour, "bt-client-recycle-max-per-hour", getEnvAsInt("HUB_BT_CLIENT_RECYCLE_MAX_PER_HOUR", 0), "Deprecated: accepted for compatibility; has no effect")

	flag.Parse()
	_, cfg.fastResumeConfigured = os.LookupEnv("HUB_FAST_RESUME_MODE")
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "fast-resume-mode" {
			cfg.fastResumeConfigured = true
		}
	})
	cfg.DeprecatedOptions = deprecatedOptions(flag.CommandLine, os.LookupEnv)
	cfg.MaxTemporaryTorrents, cfg.temporaryConfigError = strconv.Atoi(maxTemporary)
	var retentionErr error
	cfg.LogRetentionDays, retentionErr = strconv.Atoi(logRetention)
	var fileErr, totalErr error
	cfg.LogMaxFileBytes, fileErr = strconv.ParseInt(logMaxFile, 10, 64)
	cfg.LogMaxTotalBytes, totalErr = strconv.ParseInt(logMaxTotal, 10, 64)
	if fileErr != nil || totalErr != nil || cfg.LogMaxFileBytes < 65536 || cfg.LogMaxTotalBytes < cfg.LogMaxFileBytes {
		cfg.logConfigError = fmt.Errorf("invalid log byte limits: file must be >= 65536 and total >= file")
	}
	if retentionErr != nil {
		cfg.logConfigError = fmt.Errorf("log retention days must be an integer between 0 and 36500")
	}
	cfg.BTSeedConfigured = true
	cfg.BTSwarmWatchdogConfigured = true
	cfg.MaxStreamsConfigured = true
	cfg.BTSwarmHardRefreshConfigured = true
	cfg.BTClientRecycleConfigured = true
	cfg.LogLevel = getEnv("HUB_LOG_LEVEL", "info")
	ApplyDefaults(cfg)

	return cfg
}

func ApplyDefaults(cfg *Config) {
	if cfg == nil {
		return
	}
	if cfg.MaxActiveStreams == 0 && !cfg.MaxStreamsConfigured {
		cfg.MaxActiveStreams = 4
	}
	if cfg.MetainfoTempDir == "" {
		cfg.MetainfoTempDir = filepath.Join(filepath.Dir(cfg.DBPath), "http-tmp")
	}
	if cfg.LogMaxFileBytes == 0 {
		cfg.LogMaxFileBytes = 16 << 20
	}
	if cfg.LogMaxTotalBytes == 0 {
		cfg.LogMaxTotalBytes = 128 << 20
	}
	// Zero disables the emulated cache window; negative inputs normalize to it.
	if cfg.StreamCacheSize < 0 {
		cfg.StreamCacheSize = 0
	}
	if cfg.LogDir == "" {
		cfg.LogDir = filepath.Join(filepath.Dir(cfg.DBPath), "logs")
	}
	if !cfg.BTSeedConfigured {
		cfg.BTSeed = true
	}
	if cfg.BTClientProfile == "" {
		cfg.BTClientProfile = "qbittorrent"
	}
	applyDownloadProfileDefaults(cfg)
	if cfg.BTRetrackersMode == "" {
		cfg.BTRetrackersMode = "append"
	}
	if cfg.BTRetrackersFile == "" {
		cfg.BTRetrackersFile = "/config/trackers.txt"
	}
	if cfg.BTEstablishedConns <= 0 {
		cfg.BTEstablishedConns = 120
	}
	if cfg.BTHalfOpenConns <= 0 {
		cfg.BTHalfOpenConns = 80
	}
	if cfg.BTTotalHalfOpen <= 0 {
		cfg.BTTotalHalfOpen = 1000
	}
	if cfg.BTPeersLowWater <= 0 {
		cfg.BTPeersLowWater = 500
	}
	if cfg.BTPeersHighWater <= 0 {
		cfg.BTPeersHighWater = 1200
	}
	if cfg.BTDialRateLimit <= 0 {
		cfg.BTDialRateLimit = 60
	}
	if cfg.BTPeersHighWater < cfg.BTPeersLowWater {
		cfg.BTPeersHighWater = cfg.BTPeersLowWater
	}
	if !cfg.BTSwarmWatchdogConfigured {
		cfg.BTSwarmWatchdogEnabled = true
	}
	if cfg.BTSwarmCheckIntervalSec <= 0 {
		cfg.BTSwarmCheckIntervalSec = 60
	}
	if cfg.BTSwarmRefreshCooldownSec <= 0 {
		cfg.BTSwarmRefreshCooldownSec = 180
	}
	if cfg.BTSwarmMinConnectedPeers <= 0 {
		cfg.BTSwarmMinConnectedPeers = 8
	}
	if cfg.BTSwarmMinConnectedSeeds <= 0 {
		cfg.BTSwarmMinConnectedSeeds = 2
	}
	if cfg.BTSwarmStalledSpeedBps <= 0 {
		cfg.BTSwarmStalledSpeedBps = 32768
	}
	if cfg.BTSwarmStalledDurationSec <= 0 {
		cfg.BTSwarmStalledDurationSec = 180
	}
	if cfg.BTSwarmBoostConns <= 0 {
		cfg.BTSwarmBoostConns = 120
	}
	if cfg.BTSwarmBoostDurationSec <= 0 {
		cfg.BTSwarmBoostDurationSec = 300
	}
	if cfg.BTSwarmBoostConns <= cfg.BTEstablishedConns {
		cfg.BTSwarmBoostConns = cfg.BTEstablishedConns * 2
	}
	cfg.BTSwarmPeerDropRatio = clampRatio(cfg.BTSwarmPeerDropRatio, 0.45)
	cfg.BTSwarmSeedDropRatio = clampRatio(cfg.BTSwarmSeedDropRatio, 0.45)
	cfg.BTSwarmSpeedDropRatio = clampRatio(cfg.BTSwarmSpeedDropRatio, 0.35)
	if cfg.BTSwarmPeakTTLSec <= 0 {
		cfg.BTSwarmPeakTTLSec = 1800
	}

}

func (cfg *Config) ValidateLogging() error {
	if cfg.logConfigError != nil {
		return cfg.logConfigError
	}
	if cfg.LogRetentionDays < 0 || cfg.LogRetentionDays > 36500 {
		return fmt.Errorf("log retention days must be between 0 and 36500")
	}
	if cfg.LogMaxFileBytes < 65536 || cfg.LogMaxTotalBytes < cfg.LogMaxFileBytes {
		return fmt.Errorf("invalid log byte limits: file must be >= 65536 and total >= file")
	}
	return nil
}

func (cfg *Config) ValidateTemporaryTorrents() error {
	if cfg.temporaryConfigError != nil || cfg.MaxTemporaryTorrents < 1 {
		return fmt.Errorf("max temporary torrents must be a positive integer")
	}
	return nil
}

func applyDownloadProfileDefaults(cfg *Config) {
	profile := normalizedDownloadProfile(cfg.BTDownloadProfile)
	cfg.BTDownloadProfile = profile

	type defaults struct {
		established int
		halfOpen    int
		totalHalf   int
		lowWater    int
		highWater   int
		dialRate    int
	}
	values := defaults{established: 200, halfOpen: 200, totalHalf: 2000, lowWater: 700, highWater: 2500, dialRate: 200}
	switch profile {
	case "torrserver":
		values = defaults{established: 100, halfOpen: 80, totalHalf: 800, lowWater: 300, highWater: 1000, dialRate: 60}
	case "aggressive":
		values = defaults{established: 200, halfOpen: 200, totalHalf: 2000, lowWater: 700, highWater: 2500, dialRate: 200}
	}
	if cfg.BTEstablishedConns <= 0 {
		cfg.BTEstablishedConns = values.established
	}
	if cfg.BTHalfOpenConns <= 0 {
		cfg.BTHalfOpenConns = values.halfOpen
	}
	if cfg.BTTotalHalfOpen <= 0 {
		cfg.BTTotalHalfOpen = values.totalHalf
	}
	if cfg.BTPeersLowWater <= 0 {
		cfg.BTPeersLowWater = values.lowWater
	}
	if cfg.BTPeersHighWater <= 0 {
		cfg.BTPeersHighWater = values.highWater
	}
	if cfg.BTDialRateLimit <= 0 {
		cfg.BTDialRateLimit = values.dialRate
	}
}

func normalizedDownloadProfile(profile string) string {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "torrserver", "aggressive":
		return strings.ToLower(strings.TrimSpace(profile))
	default:
		return "balanced"
	}
}

func PublicIPStatus(value string, ipv6 bool) string {
	if strings.TrimSpace(value) == "" {
		return "disabled"
	}
	ip := net.ParseIP(strings.TrimSpace(value))
	if ip == nil || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return "invalid"
	}
	if ipv6 {
		if ip.To4() != nil || ip.To16() == nil {
			return "invalid"
		}
		return "configured"
	}
	if ip.To4() == nil {
		return "invalid"
	}
	return "configured"
}

func clampRatio(value, fallback float64) float64 {
	if value <= 0 {
		value = fallback
	}
	if value < 0.05 {
		return 0.05
	}
	if value > 0.95 {
		return 0.95
	}
	return value
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

func getEnvAsFloat(key string, fallback float64) float64 {
	if valueStr, exists := os.LookupEnv(key); exists {
		if value, err := strconv.ParseFloat(valueStr, 64); err == nil {
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

func (cfg *Config) ValidateResources() error {
	if (cfg.FastResumeMode != "" || cfg.fastResumeConfigured) && cfg.FastResumeMode != "fast" && cfg.FastResumeMode != "verify" {
		return fmt.Errorf("fast-resume-mode must be fast or verify")
	}
	if cfg.MaxActiveStreams <= 0 {
		return fmt.Errorf("max-streams must be positive")
	}
	if _, err := metafetch.New(cfg.MetainfoAllowlist); err != nil {
		return err
	}
	return nil
}
