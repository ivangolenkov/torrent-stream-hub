package config

import (
	"flag"
	"net"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port                               string
	TorrentPort                        int
	DownloadDir                        string
	DBPath                             string
	MaxActiveStreams                   int
	MaxActiveDownloads                 int
	MinFreeSpaceGB                     int
	DownloadLimit                      int
	UploadLimit                        int
	StreamCacheSize                    int64
	AuthEnabled                        bool
	AuthUser                           string
	AuthPassword                       string
	LogLevel                           string
	BTSeed                             bool
	BTSeedConfigured                   bool
	BTNoUpload                         bool
	BTClientProfile                    string
	BTDownloadProfile                  string
	BTBenchmarkMode                    bool
	BTPublicIPDiscoveryEnabled         bool
	BTPublicIPv4                       string
	BTPublicIPv6                       string
	BTPublicIPv4Status                 string
	BTPublicIPv6Status                 string
	BTRetrackersMode                   string
	BTRetrackersFile                   string
	BTDisableDHT                       bool
	BTDisablePEX                       bool
	BTDisableUPNP                      bool
	BTDisableTCP                       bool
	BTDisableUTP                       bool
	BTDisableIPv6                      bool
	BTEstablishedConns                 int
	BTHalfOpenConns                    int
	BTTotalHalfOpen                    int
	BTPeersLowWater                    int
	BTPeersHighWater                   int
	BTDialRateLimit                    int
	BTSwarmWatchdogEnabled             bool
	BTSwarmWatchdogConfigured          bool
	BTSwarmCheckIntervalSec            int
	BTSwarmRefreshCooldownSec          int
	BTSwarmMinConnectedPeers           int
	BTSwarmMinConnectedSeeds           int
	BTSwarmStalledSpeedBps             int
	BTSwarmStalledDurationSec          int
	BTSwarmBoostConns                  int
	BTSwarmBoostDurationSec            int
	BTSwarmPeerDropRatio               float64
	BTSwarmSeedDropRatio               float64
	BTSwarmSpeedDropRatio              float64
	BTSwarmPeakTTLSec                  int
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
	flag.StringVar(&cfg.BTDownloadProfile, "bt-download-profile", getEnv("HUB_BT_DOWNLOAD_PROFILE", "balanced"), "BitTorrent download profile: torrserver, balanced, aggressive")
	flag.BoolVar(&cfg.BTBenchmarkMode, "bt-benchmark-mode", getEnvAsBool("HUB_BT_BENCHMARK_MODE", false), "Suppress automatic recovery mutations for download benchmarking")
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
	flag.BoolVar(&cfg.BTSwarmHardRefreshEnabled, "bt-swarm-hard-refresh-enabled", getEnvAsBool("HUB_BT_SWARM_HARD_REFRESH_ENABLED", true), "Enable per-torrent runtime drop and re-add after repeated soft refresh failures")
	flag.BoolVar(&cfg.BTSwarmAutoHardRefreshEnabled, "bt-swarm-auto-hard-refresh-enabled", getEnvAsBool("HUB_BT_SWARM_AUTO_HARD_REFRESH_ENABLED", false), "Enable automatic per-torrent runtime drop and re-add; disabled by default")
	flag.IntVar(&cfg.BTSwarmHardRefreshCooldownSec, "bt-swarm-hard-refresh-cooldown", getEnvAsInt("HUB_BT_SWARM_HARD_REFRESH_COOLDOWN_SEC", 900), "Minimum seconds between hard refresh attempts per torrent")
	flag.IntVar(&cfg.BTSwarmHardRefreshAfterSoftFails, "bt-swarm-hard-refresh-after-soft-fails", getEnvAsInt("HUB_BT_SWARM_HARD_REFRESH_AFTER_SOFT_FAILS", 1), "Soft refresh attempts before hard refresh is allowed")
	flag.IntVar(&cfg.BTSwarmHardRefreshMinTorrentAgeSec, "bt-swarm-hard-refresh-min-torrent-age", getEnvAsInt("HUB_BT_SWARM_HARD_REFRESH_MIN_TORRENT_AGE_SEC", 60), "Minimum torrent runtime age before hard refresh is allowed")
	flag.IntVar(&cfg.BTSwarmDegradationEpisodeTTLSec, "bt-swarm-degradation-episode-ttl", getEnvAsInt("HUB_BT_SWARM_DEGRADATION_EPISODE_TTL_SEC", 900), "Seconds to keep a degradation episode active")
	flag.IntVar(&cfg.BTSwarmRecoveryGraceSec, "bt-swarm-recovery-grace", getEnvAsInt("HUB_BT_SWARM_RECOVERY_GRACE_SEC", 180), "Seconds of stable health before resetting degradation escalation")
	flag.BoolVar(&cfg.BTClientRecycleEnabled, "bt-client-recycle-enabled", getEnvAsBool("HUB_BT_CLIENT_RECYCLE_ENABLED", true), "Enable in-process BitTorrent client recycle fallback")
	flag.IntVar(&cfg.BTClientRecycleCooldownSec, "bt-client-recycle-cooldown", getEnvAsInt("HUB_BT_CLIENT_RECYCLE_COOLDOWN_SEC", 900), "Minimum seconds between BitTorrent client recycle attempts")
	flag.IntVar(&cfg.BTClientRecycleAfterHardFails, "bt-client-recycle-after-hard-fails", getEnvAsInt("HUB_BT_CLIENT_RECYCLE_AFTER_HARD_FAILS", 1), "Hard refresh attempts within an episode before client recycle is allowed")
	flag.IntVar(&cfg.BTClientRecycleAfterSoftFails, "bt-client-recycle-after-soft-fails", getEnvAsInt("HUB_BT_CLIENT_RECYCLE_AFTER_SOFT_FAILS", 1), "Soft refresh attempts within an episode before automatic client recycle is allowed")
	flag.IntVar(&cfg.BTClientRecycleMinTorrentAgeSec, "bt-client-recycle-min-torrent-age", getEnvAsInt("HUB_BT_CLIENT_RECYCLE_MIN_TORRENT_AGE_SEC", 60), "Minimum torrent runtime age before automatic client recycle is allowed")
	flag.IntVar(&cfg.BTClientRecycleMinTorrents, "bt-client-recycle-min-torrents", getEnvAsInt("HUB_BT_CLIENT_RECYCLE_MIN_TORRENTS", 1), "Minimum managed torrents before client recycle is allowed")
	flag.IntVar(&cfg.BTClientRecycleMaxPerHour, "bt-client-recycle-max-per-hour", getEnvAsInt("HUB_BT_CLIENT_RECYCLE_MAX_PER_HOUR", 2), "Maximum BitTorrent client recycle attempts per hour")

	flag.Parse()
	cfg.BTSeedConfigured = true
	cfg.BTSwarmWatchdogConfigured = true
	cfg.BTSwarmHardRefreshConfigured = true
	cfg.BTClientRecycleConfigured = true
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
	if !cfg.BTSwarmHardRefreshConfigured {
		cfg.BTSwarmHardRefreshEnabled = true
	}
	if cfg.BTSwarmHardRefreshCooldownSec <= 0 {
		cfg.BTSwarmHardRefreshCooldownSec = 900
	}
	if cfg.BTSwarmHardRefreshCooldownSec < cfg.BTSwarmRefreshCooldownSec {
		cfg.BTSwarmHardRefreshCooldownSec = cfg.BTSwarmRefreshCooldownSec
	}
	if cfg.BTSwarmHardRefreshAfterSoftFails <= 0 {
		cfg.BTSwarmHardRefreshAfterSoftFails = 1
	}
	if cfg.BTSwarmHardRefreshMinTorrentAgeSec <= 0 {
		cfg.BTSwarmHardRefreshMinTorrentAgeSec = 60
	}
	if cfg.BTSwarmDegradationEpisodeTTLSec <= 0 {
		cfg.BTSwarmDegradationEpisodeTTLSec = 900
	}
	if cfg.BTSwarmRecoveryGraceSec <= 0 {
		cfg.BTSwarmRecoveryGraceSec = 180
	}
	if !cfg.BTClientRecycleConfigured {
		cfg.BTClientRecycleEnabled = true
	}
	if cfg.BTClientRecycleCooldownSec <= 0 {
		cfg.BTClientRecycleCooldownSec = 900
	}
	if cfg.BTClientRecycleAfterHardFails <= 0 {
		cfg.BTClientRecycleAfterHardFails = 1
	}
	if cfg.BTClientRecycleAfterSoftFails <= 0 {
		cfg.BTClientRecycleAfterSoftFails = 1
	}
	if cfg.BTClientRecycleMinTorrentAgeSec <= 0 {
		cfg.BTClientRecycleMinTorrentAgeSec = 60
	}
	if cfg.BTClientRecycleMinTorrents <= 0 {
		cfg.BTClientRecycleMinTorrents = 1
	}
	if cfg.BTClientRecycleMaxPerHour <= 0 {
		cfg.BTClientRecycleMaxPerHour = 2
	}
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
