package usecase

import (
	"errors"

	"github.com/anacrolix/torrent"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/models"
)

// Caller owns the record/root handles. A committed version is not an applied
// version: every command uses the same apply/bind/failure boundary. Staged
// commands have already closed gates and must record a failed commit as well.
func (uc *TorrentUseCase) runIntentCommand(before models.IntentVersion, full, staged bool, commit func() (models.IntentVersion, error), apply func() error) (models.IntentVersion, error) {
	expected := uc.engine.GetRawTorrent(before.Hash)
	v, err := commit()
	if err != nil {
		if staged {
			err = uc.failedIntentApply(expected, before, "intent_commit_failed", err)
		}
		return before, err
	}
	if !full && v == before {
		// A metadata/policy no-op did not apply the previous command, even when
		// that previous command happens to be exactly one revision ahead.
		return v, nil
	}
	return v, uc.applyCommittedIntent(expected, v, full, apply)
}

func (uc *TorrentUseCase) applyCommittedIntent(expected *torrent.Torrent, v models.IntentVersion, full bool, apply func() error) error {
	if apply != nil {
		err := apply()
		if err != nil && !(expected == nil && errors.Is(err, engine.ErrTorrentNotFound)) {
			return uc.failedIntentApply(expected, v, "runtime_apply_failed", err)
		}
	}
	bind := bindIncrementalRuntime
	if full {
		bind = bindAppliedRuntime
	}
	if err := bind(uc.engine, uc.repo, v); err != nil {
		return uc.failedIntentApply(expected, v, "runtime_bind_failed", err)
	}
	return nil
}
