package usecase

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/text/unicode/norm"
	"os"
	"strings"
	"time"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"
)

var ErrOwnershipCatalogChanged = errors.New("storage ownership catalog changed; retry required")

type ownershipClaim struct {
	hash  string
	paths []string
}

func ownershipOverlap(a, b string) bool {
	return pathsOverlap(strings.ToLower(norm.NFC.String(a)), strings.ToLower(norm.NFC.String(b)))
}

// Reservations outlive preflight and protect through the last filesystem effect.
// Only the registry mutation and short mapping publication use ownershipMu.
func (uc *TorrentUseCase) reserveOwnership(ctx context.Context, root *os.Root, hash string, paths []string) (func(), error) {
	for _, path := range paths {
		if err := uc.checkVolume(path); err != nil {
			return nil, err
		}
	}
	for attempt := 0; attempt < 16; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		snapshot, err := uc.repo.ReadOwnership(ctx)
		if err != nil {
			return nil, err
		}
		if uc.beforeOwnershipValidate != nil {
			uc.beforeOwnershipValidate()
		}
		err = uc.validateOwnershipSnapshot(ctx, root, hash, paths, snapshot)
		uc.ownershipMu.Lock()
		version, versionErr := uc.repo.OwnershipVersion(ctx)
		if versionErr != nil {
			uc.ownershipMu.Unlock()
			return nil, versionErr
		}
		if version == snapshot.Version {
			if err == nil {
				err = uc.claimsConflictLocked(hash, paths)
			}
			if err != nil {
				uc.ownershipMu.Unlock()
				return nil, err
			}
			claim := &ownershipClaim{hash: hash, paths: append([]string(nil), paths...)}
			if uc.ownershipClaims == nil {
				uc.ownershipClaims = make(map[*ownershipClaim]bool)
			}
			uc.ownershipClaims[claim] = true
			uc.ownershipMu.Unlock()
			return func() { uc.ownershipMu.Lock(); delete(uc.ownershipClaims, claim); uc.ownershipMu.Unlock() }, nil
		}
		uc.ownershipMu.Unlock()
		timer := time.NewTimer(time.Duration(attempt+1) * time.Millisecond * 5)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, ErrOwnershipCatalogChanged
}
func (uc *TorrentUseCase) claimsConflictLocked(hash string, paths []string) error {
	for claim := range uc.ownershipClaims {
		if claim.hash == hash {
			continue
		} // same-record effects already serialize on command/work tickets
		for _, a := range paths {
			for _, b := range claim.paths {
				if ownershipOverlap(a, b) {
					return fmt.Errorf("%w: path reserved by storage operation", ErrStorageConflict)
				}
			}
		}
	}
	return nil
}
func (uc *TorrentUseCase) publishStorage(m *repository.StorageMapping, alias, ensure bool, actors ...string) error {
	uc.ownershipMu.Lock()
	defer uc.ownershipMu.Unlock()
	actor := m.Hash
	if len(actors) > 0 {
		actor = actors[0]
	}
	if err := uc.claimsConflictLocked(actor, nonemptyPaths(m.Root)); err != nil {
		return err
	}
	if alias {
		return uc.repo.SaveStorageAlias(m)
	}
	if ensure {
		return uc.repo.EnsureStorageMapping(m)
	}
	return uc.repo.SaveStorageMapping(m)
}
func (uc *TorrentUseCase) claimStorageAlias(alias, owner string) error {
	uc.ownershipMu.Lock()
	defer uc.ownershipMu.Unlock()
	m, err := uc.repo.StorageMapping(owner)
	if err != nil {
		return err
	}
	if m == nil {
		return ErrStorageConflict
	}
	if err := uc.claimsConflictLocked(owner, nonemptyPaths(m.Root)); err != nil {
		return err
	}
	return uc.repo.ClaimStorageAlias(alias, owner)
}

func (uc *TorrentUseCase) snapshotStoragePaths(rec repository.OwnershipRecord) ([]string, error) {
	if err := checkMappingVersion(rec.Mapping); err != nil {
		return nil, err
	}
	if rec.Pending && rec.CleanupReady {
		return rec.Cleanup, nil
	}
	info, err := uc.metadataStorageInfo(rec.Torrent)
	if err != nil {
		return nil, err
	}
	m := rec.Mapping
	if info == nil {
		if m != nil && payload.Supported(m.Version) {
			if m.Root == "" {
				return nil, nil
			}
			return []string{m.Root}, nil
		}
		return nil, nil
	}
	paths, err := storagePaths(info)
	if err != nil {
		return nil, err
	}
	if m != nil && payload.Supported(m.Version) {
		for i, p := range paths {
			paths[i], err = m.Layout().Path(p)
			if err != nil {
				return nil, err
			}
		}
	}
	return paths, nil
}

func nonemptyPaths(path string) []string {
	if path == "" {
		return nil
	}
	return []string{path}
}
