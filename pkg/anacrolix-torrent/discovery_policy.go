package torrent

// PublicDiscoveryAllowed covers manual and automatic discovery. A metadata
// private flag is authoritative as soon as it arrives; PrivateSafe also covers
// the period before metadata when no such inference is possible.
func (t *Torrent) PublicDiscoveryAllowed() bool {
	t.cl.rLock()
	defer t.cl.rUnlock()
	return t.publicDiscoveryAllowedLocked()
}

func (t *Torrent) publicDiscoveryAllowedLocked() bool {
	return !t.privateSafe && !t.metadataPrivate && (t.info == nil || t.info.Private == nil || !*t.info.Private)
}

func publicPeerSource(source PeerSource) bool {
	return source == PeerSourceDhtGetPeers || source == PeerSourceDhtAnnouncePeer || source == PeerSourcePex || source == PeerSourceUtHolepunch
}

func (t *Torrent) enforceDiscoveryPolicyLocked() {
	if t.publicDiscoveryAllowedLocked() {
		return
	}
	if t.stopPublicDiscovery != nil {
		t.stopPublicDiscovery()
	}
	// Peers learned before the flag had unknown provenance. Start a fresh swarm
	// using the originally supplied trackers; public callbacks are rejected too.
	if t.peers.om != nil {
		t.peers.om.Clear(false)
	}
	for c := range t.conns {
		c.pex.Close()
		t.dropConnection(c)
	}
	t.cl.event.Broadcast()
}
