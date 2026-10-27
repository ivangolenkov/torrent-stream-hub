package storage

import (
	"context"
	"io"

	g "github.com/anacrolix/generics"

	"github.com/anacrolix/torrent/metainfo"
)

type ClientImplCloser interface {
	ClientImpl
	Close() error
}

// Represents data storage for an unspecified torrent.
type ClientImpl interface {
	OpenTorrent(ctx context.Context, info *metainfo.Info, infoHash metainfo.Hash) (TorrentImpl, error)
}

// Returning a negative cap, can we indicate there's no specific cap? If this is not-nil we use it
// as a key into piece request order. The capped bool also needs to be true to be truly capped
// though.
type TorrentCapacity *func() (cap int64, capped bool)

// Data storage bound to a torrent.
type TorrentImpl struct {
	// IOTimings returns cached counters only; it must not perform I/O or wait
	// on payload/lifecycle locks. Nil means unsupported storage.
	IOTimings func() IOTimings
	// InitialIncomplete contains ordered, disjoint half-open piece ranges found
	// missing/short during this backend open. Initial admission only: never reuse
	// it for download verification, refresh or explicit recheck. It grants no trust.
	// The client consumes and releases it before publishing metadata readiness.
	InitialIncomplete []PieceRange
	CachedRestored    func(int) bool           // I/O-free source classification for current readiness
	ResumeObservation func() ResumeObservation // immutable open outcome
	// v2 infos might not have the piece hash available even if we have the info. The
	// metainfo.Piece.Hash method was removed to enforce this.
	Piece func(p metainfo.Piece) PieceImpl
	// Preferred over PieceWithHash. Called with the piece hash if it's available.
	PieceWithHash func(p metainfo.Piece, pieceHash g.Option[[]byte]) PieceImpl
	Close         func() error
	// Storages that share the same space, will provide equal pointers. The function is called once
	// to determine the storage for torrents sharing the same function pointer, and mutated in
	// place.
	Capacity TorrentCapacity

	NewReader      func() TorrentReader
	NewPieceReader func(p Piece) PieceReader
	// Optional generation/freshness check for already verified pieces.
	FreshComplete func(piece int) bool
	// CachedComplete is memory-only; RefreshFresh performs filesystem checks
	// outside the library lock before byte delivery/readiness decisions.
	CachedComplete func(piece int) bool
	RefreshFresh   func(begin, end int)
	// Checkpoint callbacks run outside Client.lock and must not reenter it.
	WithCheckpoint    func(context.Context, []byte, func(FileCheckpoint) error) error
	PrepareRestore    func(context.Context, FileCheckpoint, func(context.Context) error) (*RestoreToken, error)
	RestoreCheckpoint func(context.Context, *RestoreToken) error
	// RevokeCheckpoint closes live freshness and durably revokes persisted trust
	// before a known-invalid hash/layer transition. It is not a completion hint.
	RevokeCheckpoint func(begin, end int) error
	RefreshEmptyFile func(index int)
	CachedEmptyFile  func(index int) bool
	// CheckpointState is cached scheduling metadata, callable under Client.lock.
	// It must not do I/O, block on mutation admission or grant payload readiness.
	CheckpointState func() (revision uint64, dirty bool)
}

type PieceRange struct{ Begin, End int }

// Interacts with torrent piece data. Optional interfaces to implement include://
//
//		io.WriterTo, such as when a piece supports a more efficient way to write out incomplete chunks.
//		SelfHashing, such as when a piece supports a more efficient way to hash its contents.
//	 	PieceReaderer when it has a stateful Reader interface that is more efficient.
type PieceImpl interface {
	// These interfaces are not as strict as normally required. They can
	// assume that the parameters are appropriate for the dimensions of the
	// piece.
	io.ReaderAt
	io.WriterAt
	// Called when the client believes the piece data will pass a hash check.
	// The storage can move or mark the piece data as read-only as it sees
	// fit.
	MarkComplete() error
	MarkNotComplete() error
	// Returns the state of a piece. Typically, this is implemented in some kind of storage to avoid
	// rehashing, and cheap checks are performed here. (The implementation maintains a cache in
	// Torrent).
	Completion() Completion
}

// Completion state of a piece.
type Completion struct {
	Err error
	// The state is known or cached.
	Ok bool
	// If Ok, whether the data is correct. TODO: Check all callsites test Ok first.
	Complete bool
}

// Allows a storage backend to override hashing (i.e. if it can do it more efficiently than the
// torrent client can).
type SelfHashing interface {
	SelfHash() (metainfo.Hash, error)
}

// Piece supports dedicated reader.
type PieceReaderer interface {
	NewReader() (PieceReader, error)
}

type PieceReader interface {
	io.ReaderAt
	io.Closer
}
