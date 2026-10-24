package engine

import "time"

func (e *Engine) now() time.Time {
	if e.clock != nil {
		return e.clock()
	}
	return time.Now()
}

// admissionMu serializes boost policy, expiry and quiesce with the generation.
func (e *Engine) startBoostLocked(mt *ManagedTorrent) { e.startBoostAtLocked(mt, e.now()) }

func (e *Engine) startBoostAtLocked(mt *ManagedTorrent, now time.Time) {
	mt.mu.Lock()
	already := !mt.boostUntil.IsZero()
	duration := time.Duration(e.cfg.BTSwarmBoostDurationSec) * time.Second
	if duration <= 0 {
		duration = 5 * time.Minute
	}
	mt.boostUntil = now.Add(duration)
	mt.mu.Unlock()
	previous := mt.t.SetMaxEstablishedConns(e.cfg.BTSwarmBoostConns)
	if !already {
		mt.mu.Lock()
		mt.normalMaxEstablishedConns = previous
		mt.mu.Unlock()
	}
}

func (e *Engine) clearBoostLocked(mt *ManagedTorrent) {
	mt.mu.Lock()
	active := !mt.boostUntil.IsZero()
	normal := mt.normalMaxEstablishedConns
	mt.boostUntil = time.Time{}
	mt.mu.Unlock()
	if active {
		if normal <= 0 {
			normal = e.cfg.BTEstablishedConns
		}
		mt.t.SetMaxEstablishedConns(normal)
	}
}

func (e *Engine) expireBoost(mt *ManagedTorrent) { e.expireBoostAt(mt, e.now()) }

func (e *Engine) expireBoostAt(mt *ManagedTorrent, now time.Time) {
	e.admissionMu.Lock()
	defer e.admissionMu.Unlock()
	if !e.currentTorrent(mt) {
		return
	}
	mt.mu.Lock()
	expired := !mt.boostUntil.IsZero() && !now.Before(mt.boostUntil)
	mt.mu.Unlock()
	if expired {
		e.clearBoostLocked(mt)
	}
}
