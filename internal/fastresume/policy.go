// Package fastresume defines the bounded checkpoint policy. It does not perform
// I/O or grant runtime readiness: an adapter must validate rooted file evidence,
// durable revocations and attachment fencing before importing a candidate.
package fastresume

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"torrent-stream-hub/internal/payload"
)

const (
	SchemaVersion            = 1
	MaxPieces                = 4 << 20
	MaxFiles                 = 4096
	MaxCheckpointBytes       = 4 << 20
	MaxPathBytes             = 1 << 20
	MaxPathLength            = 4096
	MaxPathDepth             = 128
	MaxIdentityLength        = 256
	MaxPendingOwners         = 64
	FileBatchSize            = 128
	ScratchBytes             = 128 << 10
	RestoreMemoryBytes       = 32 << 20
	GlobalWorkingMemoryBytes = 64 << 20
	CheckpointInterval       = 60 * time.Second
	QuiesceTimeout           = 2 * time.Second
)

type Mode string

const (
	Fast   Mode = "fast"
	Verify Mode = "verify"
)

// ParseMode defines the restart-scoped default. VerifyOnOpen remains enabled;
// fast trust still requires the validated import protocol, never ordinary hints.
func ParseMode(value string) (Mode, error) {
	switch Mode(value) {
	case "", Fast:
		return Fast, nil
	case Verify:
		return Verify, nil
	default:
		return "", fmt.Errorf("unknown fast resume mode %q", value)
	}
}

// Identity is owner-scoped, not alias-hash-scoped. MetainfoDigest is SHA-256 of
// the canonical owner's info bytes; CatalogDigest also binds v2 piece layers and
// the ordered physical file catalog. These are not hashes of payload contents.
type Identity struct {
	RecordID        string
	StorageID       string
	MappingRevision int64
	LayoutVersion   int
	Root            string
	Prefix          string
	MetainfoDigest  string
	CatalogDigest   string
	VolumeID        string
	PieceLength     int64
	PieceCount      int
	// ExtentLength includes v2 alignment gaps; file Length does not.
	ExtentLength int64
}

type Evidence struct {
	// Present requires a rooted regular-file open and usable native identity.
	Present    bool
	Identity   string
	Size       int64
	ModifiedNS int64
}

type File struct {
	Path     string
	Offset   int64
	Length   int64
	Evidence Evidence
}

type Checkpoint struct {
	Schema      int
	Identity    Identity
	Generation  int64
	CreatedAtMS int64
	Files       []File
	// Bitmaps are LSB-first. Bits beyond PieceCount must be zero.
	Pieces []byte
}

var (
	ErrInvalid = errors.New("invalid fast resume checkpoint")
	ErrBudget  = errors.New("fast resume checkpoint budget exceeded")
)

func bitmapBytes(pieces int) int { return (pieces + 7) / 8 }

func validBitmap(bits []byte, pieces int) bool {
	if pieces < 0 || pieces > MaxPieces || len(bits) != bitmapBytes(pieces) {
		return false
	}
	return pieces%8 == 0 || bits[len(bits)-1]>>uint(pieces%8) == 0
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (id Identity) validate() error {
	if id.PieceCount < 0 {
		return ErrInvalid
	}
	if id.PieceCount > MaxPieces {
		return ErrBudget
	}
	if id.RecordID == "" || len(id.RecordID) > MaxIdentityLength || id.StorageID == "" || len(id.StorageID) > MaxIdentityLength || id.VolumeID == "" || len(id.VolumeID) > MaxIdentityLength || id.MappingRevision <= 0 || !validDigest(id.MetainfoDigest) || !validDigest(id.CatalogDigest) {
		return ErrInvalid
	}
	if len(id.Root) > MaxPathLength || len(id.Prefix) > MaxPathLength {
		return ErrBudget
	}
	if strings.ContainsRune(id.Root, 0) || strings.ContainsRune(id.Prefix, 0) {
		return ErrInvalid
	}
	if err := (payload.Layout{Version: id.LayoutVersion, Root: id.Root, Prefix: id.Prefix}).Validate(); err != nil {
		return ErrInvalid
	}
	if id.PieceLength <= 0 || id.ExtentLength < 0 {
		return ErrInvalid
	}
	// Avoid overflow at MaxInt64; ExtentLength may include v2 alignment.
	count := id.ExtentLength / id.PieceLength
	if id.ExtentLength%id.PieceLength != 0 {
		count++
	}
	if count != int64(id.PieceCount) {
		return ErrInvalid
	}
	return nil
}

func (c *Checkpoint) Validate() error {
	if c == nil || c.Schema != SchemaVersion || c.Generation <= 0 || c.CreatedAtMS <= 0 {
		return ErrInvalid
	}
	if err := c.Identity.validate(); err != nil {
		return err
	}
	if len(c.Files) > MaxFiles {
		return ErrBudget
	}
	if len(c.Files) == 0 || !validBitmap(c.Pieces, c.Identity.PieceCount) {
		return ErrInvalid
	}
	paths := make(map[string]struct{}, len(c.Files))
	pathBytes := 0
	var end int64
	for _, f := range c.Files {
		pathBytes += len(f.Path)
		if pathBytes > MaxPathBytes || len(f.Path) > MaxPathLength || strings.Count(f.Path, string(filepath.Separator)) >= MaxPathDepth || len(f.Evidence.Identity) > MaxIdentityLength {
			return ErrBudget
		}
		if strings.ContainsRune(f.Path, 0) || payload.ValidateRelative(f.Path) != nil || filepath.Clean(f.Path) != f.Path || f.Offset < end || f.Length < 0 || f.Offset > c.Identity.ExtentLength || f.Length > c.Identity.ExtentLength-f.Offset {
			return ErrInvalid
		}
		key := payload.CollisionKey(f.Path)
		if _, exists := paths[key]; exists {
			return ErrInvalid
		}
		paths[key] = struct{}{}
		if f.Evidence.Size < 0 || f.Evidence.Present && f.Evidence.Identity == "" {
			return ErrInvalid
		}
		end = f.Offset + f.Length
	}
	for path := range paths {
		for parent := filepath.Dir(path); parent != "."; parent = filepath.Dir(parent) {
			if _, exists := paths[parent]; exists {
				return ErrInvalid
			}
		}
	}
	return nil
}

type Reason string

const (
	Restored          Reason = "restored"
	StrictMode        Reason = "strict_mode"
	NoCheckpoint      Reason = "no_checkpoint"
	InvalidCheckpoint Reason = "invalid_checkpoint"
	BudgetExceeded    Reason = "budget_exceeded"
	IdentityChanged   Reason = "identity_changed"
	StorageBlocked    Reason = "storage_blocked"
	FilesChanged      Reason = "files_changed"
	PiecesRevoked     Reason = "pieces_revoked"
)

// Candidate is a pure filtering result, NOT an authorization token. FilesChanged
// and PiecesRevoked can retain independent pieces. An empty Bits is full fallback.
type Candidate struct {
	Reason Reason
	Bits   []byte
}

// Filter only narrows persisted claims. current is the complete ordered catalog
// from metainfo plus freshly collected evidence. revoked belongs to exactly this
// checkpoint generation (verified by the durable store), not another snapshot.
// Session dirty/clean is deliberately not an input: crash alone is not a trigger.
func Filter(mode Mode, saved *Checkpoint, expected Identity, current []File, revoked []byte, blocked bool) Candidate {
	if mode != Fast {
		return Candidate{Reason: StrictMode}
	}
	if blocked {
		return Candidate{Reason: StorageBlocked}
	}
	if saved == nil {
		return Candidate{Reason: NoCheckpoint}
	}
	if err := saved.Validate(); err != nil {
		if errors.Is(err, ErrBudget) {
			return Candidate{Reason: BudgetExceeded}
		}
		return Candidate{Reason: InvalidCheckpoint}
	}
	if saved.Identity != expected || len(current) != len(saved.Files) {
		return Candidate{Reason: IdentityChanged}
	}
	if !validBitmap(revoked, expected.PieceCount) {
		return Candidate{Reason: InvalidCheckpoint}
	}
	for i, file := range current {
		old := saved.Files[i]
		if file.Path != old.Path || file.Offset != old.Offset || file.Length != old.Length {
			return Candidate{Reason: IdentityChanged}
		}
	}
	bits := bytes.Clone(saved.Pieces)
	reason := Restored
	for i := range bits {
		if bits[i]&revoked[i] != 0 {
			reason = PiecesRevoked
		}
		bits[i] &^= revoked[i]
	}
	for i, file := range current {
		old := saved.Files[i]
		if !file.Evidence.Present || file.Evidence.Size < file.Length || file.Evidence != old.Evidence {
			reason = FilesChanged
			clearFilePieces(bits, expected.PieceLength, file.Offset, file.Length)
		}
	}
	return Candidate{Reason: reason, Bits: bits}
}

func clearFilePieces(bits []byte, pieceLength, offset, length int64) {
	if length == 0 {
		return
	}
	begin := offset / pieceLength
	end := (offset + length - 1) / pieceLength
	for piece := begin; piece <= end; piece++ {
		bits[piece/8] &^= 1 << uint(piece%8)
	}
}
