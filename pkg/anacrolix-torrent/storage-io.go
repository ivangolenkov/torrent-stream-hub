package torrent

import "github.com/anacrolix/torrent/storage"

// StorageIOTimings samples cached counters for the current backend open. It
// never probes disk and does not wait on storage lifecycle/payload locks.
func (t *Torrent) StorageIOTimings() (storage.IOTimings, bool) {
	t.cl.rLock()
	defer t.cl.rUnlock()
	if t.closed.IsSet() || t.storage == nil || t.storage.IOTimings == nil {
		return storage.IOTimings{}, false
	}
	return t.storage.IOTimings(), true
}
