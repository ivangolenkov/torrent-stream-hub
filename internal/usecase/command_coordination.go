package usecase

import (
	"context"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"slices"
	"sort"

	"torrent-stream-hub/internal/payload"
)

func (uc *TorrentUseCase) lockSpec(spec *torrent.TorrentSpec) (func(), error) {
	var hashes []string
	if spec.InfoHash != (metainfo.Hash{}) {
		hashes = append(hashes, spec.InfoHash.HexString())
	}
	if spec.InfoHashV2.Ok {
		hashes = append(hashes, spec.InfoHashV2.Value.ToShort().HexString())
	}
	return uc.lockRecords(context.Background(), hashes...)
}

func (uc *TorrentUseCase) commandKeys(hashes ...string) ([]string, error) {
	var keys []string
	for _, hash := range hashes {
		keys = append(keys, "hash/"+hash, "root/"+payload.Root(hash))
		m, err := uc.repo.StorageMapping(hash)
		if err != nil {
			return nil, err
		}
		if m != nil && m.Root != "" {
			keys = append(keys, "root/"+m.Root)
		}
	}
	sort.Strings(keys)
	return slices.Compact(keys), nil
}

func (uc *TorrentUseCase) lockRecords(ctx context.Context, hashes ...string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if uc.coordinator.ctx.Err() != nil {
		return nil, context.Canceled
	}
	identities, err := uc.recordIdentities(hashes...)
	if err != nil {
		return nil, err
	}
	return uc.lockRecordsForIdentities(ctx, identities, hashes...)
}

func (uc *TorrentUseCase) recordIdentities(hashes ...string) (map[string]string, error) {
	ids := make(map[string]string, len(hashes))
	for _, hash := range hashes {
		i, err := uc.repo.GetIntent(hash)
		if err != nil {
			return nil, err
		}
		if i != nil {
			ids[hash] = i.Version.RecordID
		}
	}
	return ids, nil
}

func (uc *TorrentUseCase) sameIdentities(ids map[string]string) (bool, error) {
	for hash, id := range ids {
		i, err := uc.repo.GetIntent(hash)
		if err != nil {
			return false, err
		}
		if i == nil || i.Version.RecordID != id {
			return false, nil
		}
	}
	return true, nil
}

func (uc *TorrentUseCase) lockRecordsForIdentities(ctx context.Context, identities map[string]string, hashes ...string) (func(), error) {
	for attempt := 0; attempt < 2; attempt++ {
		keys, err := uc.commandKeys(hashes...)
		if err != nil {
			return nil, err
		}
		release, err := uc.coordinator.acquireKeys(ctx, keys...)
		if err != nil {
			return nil, err
		}
		current, err := uc.commandKeys(hashes...)
		if err == nil && slices.Equal(keys, current) {
			valid, err := uc.sameIdentities(identities)
			if err != nil || !valid {
				release()
				if err != nil {
					return nil, err
				}
				return nil, ErrStorageConflict
			}
			return release, nil
		}
		release()
		if err != nil {
			return nil, err
		}
	}
	return nil, ErrStorageConflict
}

func (uc *TorrentUseCase) lockRecord(hash string, contexts ...context.Context) (func(), error) {
	ctx := context.Background()
	if len(contexts) > 0 && contexts[0] != nil {
		ctx = contexts[0]
	}
	return uc.lockRecords(ctx, hash)
}

func (uc *TorrentUseCase) tryLockRecord(hash string) (func(), error) {
	identities, err := uc.recordIdentities(hash)
	if err != nil {
		return nil, err
	}
	keys, err := uc.commandKeys(hash)
	if err != nil {
		return nil, err
	}
	var handles []*commandReservation
	release := func() {
		for i := len(handles) - 1; i >= 0; i-- {
			handles[i].release()
		}
	}
	for _, key := range keys {
		h, err := uc.coordinator.tryAcquire(key)
		if err != nil {
			release()
			return nil, err
		}
		handles = append(handles, h)
	}
	current, err := uc.commandKeys(hash)
	if err != nil || !slices.Equal(keys, current) {
		release()
		if err != nil {
			return nil, err
		}
		return nil, ErrStorageConflict
	}
	valid, err := uc.sameIdentities(identities)
	if err != nil || !valid {
		release()
		if err != nil {
			return nil, err
		}
		return nil, ErrStorageConflict
	}
	return release, nil
}
