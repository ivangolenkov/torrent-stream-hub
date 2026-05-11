package engine

import (
	"errors"
	"net/url"
	"os"
	"strings"

	"torrent-stream-hub/internal/logging"

	"github.com/anacrolix/torrent"
)

func (e *Engine) augmentTorrentSpec(spec *torrent.TorrentSpec) {
	if spec == nil {
		return
	}
	trackers := e.retrackers()
	if len(trackers) == 0 {
		return
	}

	switch strings.ToLower(strings.TrimSpace(e.cfg.BTRetrackersMode)) {
	case "off":
		return
	case "replace":
		spec.Trackers = [][]string{trackers}
	case "", "append":
		spec.Trackers = appendTrackerTier(spec.Trackers, trackers)
	default:
		logging.Warnf("unknown retrackers mode %q, using append", e.cfg.BTRetrackersMode)
		spec.Trackers = appendTrackerTier(spec.Trackers, trackers)
	}
}

func (e *Engine) retrackers() []string {
	return mergeTrackers(defaultRetrackers(), loadTrackersFile(e.cfg.BTRetrackersFile))
}

func appendTrackerTier(existing [][]string, trackers []string) [][]string {
	if len(trackers) == 0 {
		return existing
	}
	merged := make([][]string, 0, len(existing)+1)
	for _, tier := range existing {
		cleanTier := mergeTrackers(tier)
		if len(cleanTier) > 0 {
			merged = append(merged, cleanTier)
		}
	}
	merged = append(merged, mergeTrackers(trackers))
	return merged
}

func mergeTrackers(groups ...[]string) []string {
	seen := make(map[string]bool)
	var merged []string
	for _, group := range groups {
		for _, tracker := range group {
			tracker = strings.TrimSpace(tracker)
			if tracker == "" || seen[tracker] || !validTrackerURL(tracker) {
				continue
			}
			seen[tracker] = true
			merged = append(merged, tracker)
		}
	}
	return merged
}

func validTrackerURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "udp", "http", "https", "ws", "wss":
		return true
	default:
		return false
	}
}

func loadTrackersFile(path string) []string {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logging.Warnf("failed to read retrackers file path=%s: %v", path, err)
		}
		return nil
	}
	return strings.Split(string(buf), "\n")
}

func defaultRetrackers() []string {
	return []string{
		"http://retracker.local/announce",
		"http://bt4.t-ru.org/ann?magnet",
		"http://retracker.mgts.by:80/announce",
		"http://tracker.city9x.com:2710/announce",
		"http://tracker.electro-torrent.pl:80/announce",
		"http://tracker.internetwarriors.net:1337/announce",
		"http://tracker2.itzmx.com:6961/announce",
		"udp://opentor.org:2710",
		"udp://public.popcorn-tracker.org:6969/announce",
		"udp://tracker.opentrackr.org:1337/announce",
		"http://bt.svao-ix.ru/announce",
		"udp://explodie.org:6969",
		"wss://tracker.openwebtorrent.com",
	}
}
