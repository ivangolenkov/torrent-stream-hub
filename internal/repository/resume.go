package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"

	"torrent-stream-hub/internal/fastresume"
)

var (
	ErrResumeIntegrity  = errors.New("resume checkpoint integrity failure")
	ErrResumeDurability = errors.New("resume requires SQLite WAL and FULL synchronous")
)

// Two bounded working sets (<=32 MiB each) across repository instances. Acquire
// before a SQL connection/transaction; never wait here while owning the writer.
var resumeWorkingSlots = make(chan struct{}, fastresume.GlobalWorkingMemoryBytes/fastresume.RestoreMemoryBytes)

func acquireResumeWorking(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case resumeWorkingSlots <- struct{}{}:
		return func() { <-resumeWorkingSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ResumeToken is a durable CAS fence, NOT permission to read or write payload.
// Storage adapters must additionally hold mutation/attachment ownership.
type ResumeToken struct {
	StorageID, RecordID                string
	MappingRevision, Epoch, Generation int64
}

type ResumeSnapshot struct {
	Token       ResumeToken
	Invalidated bool
	Checkpoint  *fastresume.Checkpoint
	Revoked     []byte
}

// checkResumeFence also rejects conflicting alias mappings and active journals.
// All queries use one SQL snapshot, without metainfo/filesystem I/O.
func checkResumeFence(tx sqlReader, m *StorageMapping, record string) error {
	if m == nil || !m.Ready() || record == "" {
		return ErrStateConflict
	}
	if len(m.StorageID) > fastresume.MaxIdentityLength || len(record) > fastresume.MaxIdentityLength {
		return fastresume.ErrBudget
	}
	if err := checkStorageFence(tx, m, record); err != nil {
		return err
	}
	var conflicts int
	if err := tx.QueryRow(`SELECT count(*) FROM torrent_storage WHERE storage_id=? AND (root!=? OR prefix!=? OR version!=? OR revision!=? OR state!='ready' OR manifest!='')`, m.StorageID, m.Root, m.Prefix, m.Version, m.Revision).Scan(&conflicts); err != nil {
		return err
	}
	if conflicts != 0 {
		return ErrStateConflict
	}
	var owners int
	if err := tx.QueryRow(`SELECT count(*) FROM torrent_intents i JOIN torrent_storage s ON i.hash=s.hash WHERE s.storage_id=?`, m.StorageID).Scan(&owners); err != nil {
		return err
	}
	if owners != 1 {
		return ErrStateConflict
	}
	if err := tx.QueryRow(`SELECT count(*) FROM storage_recovery r JOIN torrent_storage s ON s.hash=r.hash WHERE s.storage_id=? AND r.stage NOT IN ('planned','analyzed','completed','cancelled')`, m.StorageID).Scan(&conflicts); err != nil {
		return err
	}
	if conflicts != 0 {
		return ErrStateConflict
	}
	return nil
}

func checkResumeDurability(tx sqlReader) error {
	var mode string
	var sync int
	if err := tx.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		return err
	}
	if err := tx.QueryRow(`PRAGMA synchronous`).Scan(&sync); err != nil {
		return err
	}
	if !strings.EqualFold(mode, "wal") || sync != 2 {
		return ErrResumeDurability
	}
	return nil
}

func (r *TorrentRepo) beginResumeWrite(ctx context.Context, m *StorageMapping, record string) (*writeTx, error) {
	tx, err := r.db.BeginWrite(ctx)
	if err != nil {
		return nil, err
	}
	if err = checkResumeDurability(tx); err == nil {
		err = checkResumeFence(tx, m, record)
	}
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func readResumeHead(tx sqlReader, owner string) (*ResumeSnapshot, error) {
	s := &ResumeSnapshot{}
	err := tx.QueryRow(`SELECT
 CASE WHEN length(CAST(storage_id AS BLOB))<=256 THEN storage_id END,
 CASE WHEN length(CAST(record_id AS BLOB))<=256 THEN record_id END,
 mapping_revision,mutation_epoch,generation,invalidated FROM resume_owners WHERE storage_id=?`, owner).Scan(&s.Token.StorageID, &s.Token.RecordID, &s.Token.MappingRevision, &s.Token.Epoch, &s.Token.Generation, &s.Invalidated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err == nil && (s.Token.Epoch <= 0 || s.Token.Generation < 0 || s.Token.MappingRevision <= 0) {
		return nil, ErrResumeIntegrity
	}
	return s, err
}

// CaptureResume initializes an untrusted head or captures its current CAS fence.
// Rebinding is permitted only to the CURRENT authoritative record/mapping, and
// always advances epoch and invalidates the previous generation.
func (r *TorrentRepo) CaptureResume(ctx context.Context, m *StorageMapping, record string) (ResumeToken, error) {
	tx, err := r.beginResumeWrite(ctx, m, record)
	if err != nil {
		return ResumeToken{}, err
	}
	defer tx.Rollback()
	s, err := readResumeHead(tx, m.StorageID)
	if err != nil {
		return ResumeToken{}, err
	}
	if s == nil {
		_, err = tx.Exec(`INSERT INTO resume_owners(storage_id,record_id,mapping_revision,mutation_epoch) VALUES(?,?,?,1)`, m.StorageID, record, m.Revision)
	} else if s.Token.RecordID != record || s.Token.MappingRevision != m.Revision {
		if s.Token.Epoch == math.MaxInt64 {
			return ResumeToken{}, ErrStateConflict
		}
		_, err = tx.Exec(`UPDATE resume_owners SET record_id=?,mapping_revision=?,mutation_epoch=mutation_epoch+1,invalidated=1 WHERE storage_id=?`, record, m.Revision, m.StorageID)
	}
	if err != nil {
		return ResumeToken{}, err
	}
	s, err = readResumeHead(tx, m.StorageID)
	if err != nil {
		return ResumeToken{}, err
	}
	if err := tx.Commit(); err != nil {
		return ResumeToken{}, err
	}
	return s.Token, nil
}

func resumeIntegrity(token ResumeToken, blob, revoked []byte) []byte {
	h := sha256.New()
	h.Write([]byte("TSH-resume-integrity-1"))
	for _, s := range []string{token.StorageID, token.RecordID} {
		var size [8]byte
		binary.LittleEndian.PutUint64(size[:], uint64(len(s)))
		h.Write(size[:])
		h.Write([]byte(s))
	}
	for _, n := range []int64{token.MappingRevision, token.Epoch, token.Generation, int64(len(blob)), int64(len(revoked))} {
		var raw [8]byte
		binary.LittleEndian.PutUint64(raw[:], uint64(n))
		h.Write(raw[:])
	}
	h.Write(blob)
	h.Write(revoked)
	return h.Sum(nil)
}

// SQL CASE bounds the value returned by the driver BEFORE allocating a Go blob.
// Reading length then fetching outside the same transaction would be racy.
func readResumePayload(tx sqlReader, token ResumeToken) (blob, revoked []byte, err error) {
	var size, revokedSize int64
	var digest []byte
	err = tx.QueryRow(`SELECT length(CAST(payload AS BLOB)),length(CAST(revoked AS BLOB)),
 CASE WHEN typeof(payload)='blob' AND length(payload)<=? THEN payload END,
 CASE WHEN typeof(revoked)='blob' AND length(revoked)<=? THEN revoked END,
 CASE WHEN typeof(integrity)='blob' AND length(integrity)=32 THEN integrity END FROM resume_checkpoints WHERE storage_id=?`, fastresume.MaxCheckpointBytes, fastresume.MaxPieces/8, token.StorageID).Scan(&size, &revokedSize, &blob, &revoked, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrResumeIntegrity
	}
	if err != nil {
		return nil, nil, err
	}
	if size > fastresume.MaxCheckpointBytes || revokedSize > fastresume.MaxPieces/8 {
		return nil, nil, fastresume.ErrBudget
	}
	if !bytes.Equal(digest, resumeIntegrity(token, blob, revoked)) {
		return nil, nil, ErrResumeIntegrity
	}
	return blob, revoked, nil
}

func decodeResume(s *ResumeSnapshot, blob, revoked []byte) error {
	c, err := fastresume.Decode(blob)
	if err != nil {
		return err
	}
	i := c.Identity
	if i.StorageID != s.Token.StorageID || i.RecordID != s.Token.RecordID || i.MappingRevision != s.Token.MappingRevision || c.Generation != s.Token.Generation {
		return ErrResumeIntegrity
	}
	if err := fastresume.ValidateBitmap(revoked, i.PieceCount); err != nil {
		return err
	}
	s.Checkpoint, s.Revoked = c, revoked
	return nil
}

// LoadResume is read-only and never consumes a checkpoint on open. It does not
// check filesystem evidence or issue a runtime restore token.
func (r *TorrentRepo) LoadResume(ctx context.Context, m *StorageMapping, record string) (*ResumeSnapshot, error) {
	release, err := acquireResumeWorking(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	raw, err := r.db.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer raw.Rollback()
	tx := contextReadTx{Tx: raw, ctx: ctx}
	if err := checkResumeDurability(tx); err != nil {
		return nil, err
	}
	if err := checkResumeFence(tx, m, record); err != nil {
		return nil, err
	}
	s, err := readResumeHead(tx, m.StorageID)
	if err != nil || s == nil {
		return s, err
	}
	if s.Token.RecordID != record || s.Token.MappingRevision != m.Revision {
		return nil, ErrStateConflict
	}
	var blob, revoked []byte
	if !s.Invalidated {
		blob, revoked, err = readResumePayload(tx, s.Token)
		if err != nil {
			return nil, err
		}
	}
	if err := raw.Commit(); err != nil {
		return nil, err
	}
	if !s.Invalidated {
		if err := decodeResume(s, blob, revoked); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func checkResumeToken(tx sqlReader, token ResumeToken) (*ResumeSnapshot, error) {
	s, err := readResumeHead(tx, token.StorageID)
	if err != nil {
		return nil, err
	}
	if s == nil || s.Token != token || token.Epoch <= 0 || token.Epoch == math.MaxInt64 {
		return nil, ErrStateConflict
	}
	return s, nil
}

// CommitResume publishes a snapshot AFTER the caller has quiesced and flushed
// the payload. An old epoch can never reset revocations committed by a writer.
func (r *TorrentRepo) CommitResume(ctx context.Context, m *StorageMapping, token ResumeToken, c *fastresume.Checkpoint) (ResumeToken, error) {
	if c == nil || m == nil || token.Generation == math.MaxInt64 || c.Generation != token.Generation+1 || c.Identity.RecordID != token.RecordID || c.Identity.StorageID != token.StorageID || c.Identity.MappingRevision != token.MappingRevision || c.Identity.LayoutVersion != m.Version || c.Identity.Root != m.Root || c.Identity.Prefix != m.Prefix {
		return ResumeToken{}, ErrStateConflict
	}
	release, err := acquireResumeWorking(ctx)
	if err != nil {
		return ResumeToken{}, err
	}
	defer release()
	blob, err := fastresume.Encode(c)
	if err != nil {
		return ResumeToken{}, err
	}
	tx, err := r.beginResumeWrite(ctx, m, token.RecordID)
	if err != nil {
		return ResumeToken{}, err
	}
	defer tx.Rollback()
	if token.StorageID != m.StorageID || token.MappingRevision != m.Revision {
		return ResumeToken{}, ErrStateConflict
	}
	if _, err := checkResumeToken(tx, token); err != nil {
		return ResumeToken{}, err
	}
	next := token
	next.Generation++
	next.Epoch++
	revoked := make([]byte, len(c.Pieces))
	if _, err := tx.Exec(`INSERT INTO resume_checkpoints(storage_id,payload,revoked,integrity) VALUES(?,?,?,?) ON CONFLICT(storage_id) DO UPDATE SET payload=excluded.payload,revoked=excluded.revoked,integrity=excluded.integrity`, token.StorageID, blob, revoked, resumeIntegrity(next, blob, revoked)); err != nil {
		return ResumeToken{}, err
	}
	if _, err := tx.Exec(`UPDATE resume_owners SET generation=?,mutation_epoch=?,invalidated=0 WHERE storage_id=?`, next.Generation, next.Epoch, next.StorageID); err != nil {
		return ResumeToken{}, err
	}
	if err := tx.Commit(); err != nil {
		return ResumeToken{}, err
	}
	return next, nil
}

// RevokeResume must return success before the caller admits any corresponding
// payload write. Repeated already-revoked bits do not amplify SQL writes.
func (r *TorrentRepo) RevokeResume(ctx context.Context, m *StorageMapping, token ResumeToken, mask []byte) (ResumeToken, error) {
	release, err := acquireResumeWorking(ctx)
	if err != nil {
		return ResumeToken{}, err
	}
	defer release()
	tx, err := r.beginResumeWrite(ctx, m, token.RecordID)
	if err != nil {
		return ResumeToken{}, err
	}
	defer tx.Rollback()
	if token.StorageID != m.StorageID || token.MappingRevision != m.Revision {
		return ResumeToken{}, ErrStateConflict
	}
	s, err := checkResumeToken(tx, token)
	if err != nil {
		return ResumeToken{}, err
	}
	if s.Invalidated {
		return token, tx.Commit()
	}
	blob, revoked, err := readResumePayload(tx, token)
	if err != nil {
		return ResumeToken{}, err
	}
	if err := decodeResume(s, blob, revoked); err != nil {
		return ResumeToken{}, err
	}
	if err := fastresume.ValidateBitmap(mask, s.Checkpoint.Identity.PieceCount); err != nil {
		return ResumeToken{}, err
	}
	changed := false
	for i := range revoked {
		bits := mask[i] & s.Checkpoint.Pieces[i]
		changed = changed || bits&^revoked[i] != 0
		revoked[i] |= bits
	}
	if !changed {
		return token, tx.Commit()
	}
	next := token
	next.Epoch++
	if _, err := tx.Exec(`UPDATE resume_checkpoints SET revoked=?,integrity=? WHERE storage_id=?`, revoked, resumeIntegrity(next, blob, revoked), next.StorageID); err != nil {
		return ResumeToken{}, err
	}
	if _, err := tx.Exec(`UPDATE resume_owners SET mutation_epoch=? WHERE storage_id=?`, next.Epoch, next.StorageID); err != nil {
		return ResumeToken{}, err
	}
	if err := tx.Commit(); err != nil {
		return ResumeToken{}, err
	}
	return next, nil
}

// InvalidateResume revokes the whole owner, even when the serialized checkpoint
// is corrupt. It is also the explicit-recheck/strict-mode barrier.
func (r *TorrentRepo) InvalidateResume(ctx context.Context, m *StorageMapping, token ResumeToken) (ResumeToken, error) {
	tx, err := r.beginResumeWrite(ctx, m, token.RecordID)
	if err != nil {
		return ResumeToken{}, err
	}
	defer tx.Rollback()
	if token.StorageID != m.StorageID || token.MappingRevision != m.Revision {
		return ResumeToken{}, ErrStateConflict
	}
	if _, err := checkResumeToken(tx, token); err != nil {
		return ResumeToken{}, err
	}
	next := token
	next.Epoch++
	if _, err := tx.Exec(`UPDATE resume_owners SET invalidated=1,mutation_epoch=? WHERE storage_id=?`, next.Epoch, next.StorageID); err != nil {
		return ResumeToken{}, fmt.Errorf("invalidate resume: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ResumeToken{}, err
	}
	return next, nil
}
