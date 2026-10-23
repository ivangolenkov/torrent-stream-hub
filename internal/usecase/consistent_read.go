package usecase

import (
	"errors"
	"fmt"
	"reflect"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/repository"
)

var ErrSnapshotConflict = errors.New("torrent snapshot changed during capture")

// Each readDB call is an existing consistent SQLite read transaction. Compare
// the durable snapshot around the immutable runtime capture and validate runtime
// identity/policy/binding afterwards. Two attempts bound churn; failure is an
// explicit conflict, never a guessed policy or falsely empty publication.
//
// Library counters/activity are sampled telemetry, not a distributed transaction
// with SQLite. Their stamps must never be used as readiness or command authority.
func readConsistentTorrents(readDB func() ([]*models.Torrent, error), readRuntime func() []*models.Torrent, captureValidation func() func() bool) ([]*models.Torrent, error) {
	for attempt := 0; attempt < 2; attempt++ {
		before, err := readDB()
		if err != nil {
			return nil, err
		}
		validate := captureValidation()
		runtime := readRuntime()
		after, err := readDB()
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(before, after) || !validate() {
			continue
		}
		return projectTorrents(runtime, after), nil
	}
	return nil, fmt.Errorf("%w: %w", ErrSnapshotConflict, repository.ErrStateConflict)
}

func (uc *TorrentUseCase) readAllConsistently() ([]*models.Torrent, error) {
	return readConsistentTorrents(uc.repo.GetAllTorrents, uc.engine.GetAllTorrents, func() func() bool {
		stamp := uc.engine.CaptureReadStamp("")
		return func() bool { return uc.engine.ValidateReadStamp(stamp) }
	})
}
