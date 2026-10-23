package repository

import "errors"

var (
	ErrTorrentNotFound  = errors.New("torrent not found")
	ErrFileNotFound     = errors.New("file not found")
	ErrInvalidPriority  = errors.New("invalid file priority")
	ErrMetadataPending  = errors.New("file metadata is not available")
	ErrStateConflict    = errors.New("record generation or revision conflicts with current state")
	ErrStaleObservation = errors.New("runtime observation is obsolete")
)
