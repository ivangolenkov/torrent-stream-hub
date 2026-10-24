package engine

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"torrent-stream-hub/internal/config"
)

func TestPrivateAndUnknownMetadataNeverReceiveRetrackers(t *testing.T) {
	e := &Engine{cfg: &config.Config{BTRetrackersMode: "replace", BTRetrackersFile: filepath.Join(t.TempDir(), "none")}}
	private := true
	data, err := bencode.Marshal(metainfo.Info{Name: "private", Length: 1, PieceLength: 1, Pieces: make([]byte, 20), Private: &private})
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range [][]byte{nil, data} {
		spec := &torrent.TorrentSpec{Trackers: [][]string{{"https://private.example/announce"}}}
		spec.InfoBytes = info
		e.augmentTorrentSpec(spec)
		if len(spec.Trackers) != 1 || len(spec.Trackers[0]) != 1 || spec.Trackers[0][0] != "https://private.example/announce" {
			t.Fatal("private trackers replaced", spec.Trackers)
		}
	}
}

func TestLowUploadLimitCanReserveProtocolBlock(t *testing.T) {
	cfg := buildClientConfig(&config.Config{UploadLimit: 1024})
	limiter := cfg.UploadRateLimiter
	if limiter.Limit() != 1024 || limiter.Burst() < 64<<10 {
		t.Fatal("invalid rate/burst", limiter.Limit(), limiter.Burst())
	}
	now := time.Now()
	if !limiter.AllowN(now, limiter.Burst()) {
		t.Fatal("cannot consume initial burst")
	}
	r := limiter.ReserveN(now, 16<<10)
	if !r.OK() || r.DelayFrom(now) < 16*time.Second {
		t.Fatal("block rejected or sustained limit bypassed", r.OK(), r.DelayFrom(now))
	}
	r.CancelAt(now)
}

func TestBoostExpiresExtendsAndCannotAffectReplacement(t *testing.T) {
	e, tor := setupCompletedLifecycleTorrent(t)
	e.mu.RLock()
	mt := e.managedTorrents[tor.Hash]
	e.mu.RUnlock()
	now := time.Now().Add(time.Hour)
	// Exercise the controlled-clock entry points without wall-clock sleeps.
	e.admissionMu.Lock()
	e.startBoostAtLocked(mt, now)
	e.admissionMu.Unlock()
	mt.mu.Lock()
	first := mt.boostUntil
	mt.mu.Unlock()
	if first.IsZero() {
		t.Fatal("missing boost deadline")
	}
	e.admissionMu.Lock()
	e.startBoostAtLocked(mt, now.Add(time.Minute))
	e.admissionMu.Unlock()
	mt.mu.Lock()
	if mt.boostUntil.Before(first) {
		t.Fatal("boost shortened")
	}
	mt.mu.Unlock()
	e.expireBoostAt(mt, now.Add(time.Hour))
	mt.mu.Lock()
	until := mt.boostUntil
	normal := mt.normalMaxEstablishedConns
	mt.mu.Unlock()
	if !until.IsZero() {
		t.Fatal("boost did not expire")
	}
	if previous := mt.t.SetMaxEstablishedConns(normal); previous != normal {
		t.Fatal("normal limit not restored", previous, normal)
	}
	if err := e.Delete(tor.Hash); err != nil {
		t.Fatal(err)
	}
	e.expireBoostAt(mt, now.Add(time.Hour))
}
