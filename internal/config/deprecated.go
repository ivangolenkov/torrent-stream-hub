package config

import (
	"flag"
	"sort"
)

// Keep legacy inputs parseable through phase 40. Remove only in a versioned
// configuration change, with migration notes; never silently reactivate them.
var deprecatedSettings = map[string]string{
	"bt-benchmark-mode":                      "HUB_BT_BENCHMARK_MODE",
	"bt-swarm-hard-refresh-enabled":          "HUB_BT_SWARM_HARD_REFRESH_ENABLED",
	"bt-swarm-auto-hard-refresh-enabled":     "HUB_BT_SWARM_AUTO_HARD_REFRESH_ENABLED",
	"bt-swarm-hard-refresh-cooldown":         "HUB_BT_SWARM_HARD_REFRESH_COOLDOWN_SEC",
	"bt-swarm-hard-refresh-after-soft-fails": "HUB_BT_SWARM_HARD_REFRESH_AFTER_SOFT_FAILS",
	"bt-swarm-hard-refresh-min-torrent-age":  "HUB_BT_SWARM_HARD_REFRESH_MIN_TORRENT_AGE_SEC",
	"bt-swarm-degradation-episode-ttl":       "HUB_BT_SWARM_DEGRADATION_EPISODE_TTL_SEC",
	"bt-swarm-recovery-grace":                "HUB_BT_SWARM_RECOVERY_GRACE_SEC",
	"bt-client-recycle-enabled":              "HUB_BT_CLIENT_RECYCLE_ENABLED",
	"bt-client-recycle-cooldown":             "HUB_BT_CLIENT_RECYCLE_COOLDOWN_SEC",
	"bt-client-recycle-after-hard-fails":     "HUB_BT_CLIENT_RECYCLE_AFTER_HARD_FAILS",
	"bt-client-recycle-after-soft-fails":     "HUB_BT_CLIENT_RECYCLE_AFTER_SOFT_FAILS",
	"bt-client-recycle-min-torrent-age":      "HUB_BT_CLIENT_RECYCLE_MIN_TORRENT_AGE_SEC",
	"bt-client-recycle-min-torrents":         "HUB_BT_CLIENT_RECYCLE_MIN_TORRENTS",
	"bt-client-recycle-max-per-hour":         "HUB_BT_CLIENT_RECYCLE_MAX_PER_HOUR",
}

func deprecatedOptions(fs *flag.FlagSet, lookup func(string) (string, bool)) []string {
	var names []string
	fs.Visit(func(f *flag.Flag) {
		if _, ok := deprecatedSettings[f.Name]; ok {
			names = append(names, "--"+f.Name)
		}
	})
	for _, env := range deprecatedSettings {
		if _, set := lookup(env); set {
			names = append(names, env)
		}
	}
	sort.Strings(names)
	return names
}
