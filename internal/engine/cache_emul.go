package engine

import (
	"fmt"
)

// CacheStatus represents the emulated sliding window cache status
type CacheStatus struct {
	VirtualCacheBytes int64 `json:"virtual_cache_bytes"`
	IsFullyDownloaded bool  `json:"is_fully_downloaded"`
}

// GetCacheStatus calculates the sliding window of continuously downloaded bytes ahead of the current offset.
func (e *Engine) GetCacheStatus(hash string, fileIndex int, currentOffset int64) (*CacheStatus, error) {
	e.mu.RLock()
	mt, ok := e.managedTorrents[hash]
	e.mu.RUnlock()

	if !ok {
		return nil, TorrentNotFoundError{Hash: hash}
	}
	if mt.t.Info() == nil {
		return nil, fmt.Errorf("torrent metadata is not available yet")
	}

	files := mt.t.Files()
	if fileIndex < 0 || fileIndex >= len(files) {
		return nil, fmt.Errorf("file index out of bounds: %d", fileIndex)
	}

	file := files[fileIndex]

	// Optimization: if fully downloaded, skip bitfield calculations
	if file.BytesCompleted() == file.Length() {
		return &CacheStatus{
			VirtualCacheBytes: file.Length() - currentOffset,
			IsFullyDownloaded: true,
		}, nil
	}

	// Calculate which piece corresponds to the currentOffset
	pieceLength := mt.t.Info().PieceLength
	startPiece := (file.Offset() + currentOffset) / int64(pieceLength)
	endPiece := (file.Offset() + file.Length() - 1) / int64(pieceLength)

	var continuousPieces int64 = 0

	// Check bitfield forwards to count continuous pieces
	for p := startPiece; p <= endPiece; p++ {
		piece := mt.t.Piece(int(p))
		if piece.State().Complete {
			continuousPieces++
		} else {
			break
		}
	}

	// Convert continuous pieces to bytes
	virtualCacheBytes := continuousPieces * int64(pieceLength)

	// Apply HUB_STREAM_CACHE_SIZE Fake Limit
	if virtualCacheBytes > e.cfg.StreamCacheSize {
		virtualCacheBytes = e.cfg.StreamCacheSize
	}

	// Make sure we don't exceed the remaining file size
	remainingBytes := file.Length() - currentOffset
	if virtualCacheBytes > remainingBytes {
		virtualCacheBytes = remainingBytes
	}

	// In case we are completely in an un-downloaded zone, it's 0.
	if virtualCacheBytes < 0 {
		virtualCacheBytes = 0
	}

	return &CacheStatus{
		VirtualCacheBytes: virtualCacheBytes,
		IsFullyDownloaded: false,
	}, nil
}
