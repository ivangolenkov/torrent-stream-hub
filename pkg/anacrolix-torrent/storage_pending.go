package torrent

// PendingStorageMetadata returns only hash-validated metadata held while the
// application reserves a physical location. No payload storage is open yet.
func (t *Torrent) PendingStorageMetadata() []byte {
	t.cl.rLock()
	defer t.cl.rUnlock()
	if !t.storagePending || t.closed.IsSet() {
		return nil
	}
	return append([]byte(nil), t.metadataBytes...)
}
