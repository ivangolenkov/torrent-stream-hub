package usecase

import (
	"errors"
	"fmt"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
)

var ErrSnapshotConflict = errors.New("torrent snapshot changed during capture")

// Materialize one SQLite read transaction between cheap durable version reads.
// Validate runtime identity/policy/binding around the immutable runtime capture. Two attempts bound churn; failure is an
// explicit conflict, never a guessed policy or falsely empty publication.
//
// Library counters/activity are sampled telemetry, not a distributed transaction
// with SQLite. Their stamps must never be used as readiness or command authority.
func readConsistentTorrents(readVersion func() (int64, error), readDB func() ([]*models.Torrent, error), readRuntime func() []*models.Torrent, captureValidation func() func() bool) ([]*models.Torrent, error) {
	for attempt := 0; attempt < 2; attempt++ {
		version, err := readVersion()
		if err != nil {
			return nil, err
		}
		before, err := readDB()
		if err != nil {
			return nil, err
		}
		validate := captureValidation()
		runtime := readRuntime()
		after, err := readVersion()
		if err != nil {
			return nil, err
		}
		if version != after || !validate() {
			continue
		}
		return projectTorrents(runtime, before), nil
	}
	return nil, fmt.Errorf("%w: %w", ErrSnapshotConflict, repository.ErrStateConflict)
}

func (uc *TorrentUseCase) readAllConsistently() ([]*models.Torrent, error) {
	return readConsistentTorrents(func() (int64, error) { return uc.repo.ReadVersion() }, uc.repo.GetAllTorrents, uc.engine.GetAllTorrents, func() func() bool {
		stamp := uc.engine.CaptureReadStamp("")
		return func() bool { return uc.engine.ValidateReadStamp(stamp) }
	})
}
