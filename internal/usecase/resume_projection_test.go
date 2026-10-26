package usecase

import (
	"testing"
	"torrent-stream-hub/internal/models"
)

func TestResumeDiagnosisCannotCrossRecordOrAttachment(t *testing.T) {
	for _, control := range []*models.TorrentControl{
		nil,
		{RecordID: "old", RuntimeAttachment: "stale"},
		{RecordID: "current"},
	} {
		live := &models.Torrent{Hash: testInfoHash, Control: control, Resume: &models.ResumeProgress{Mode: "fast", Status: "restored", RestoredBytes: 32}}
		saved := &models.Torrent{Hash: testInfoHash, Control: &models.TorrentControl{RecordID: "current", DesiredRun: models.RunPaused}}
		view := projectTorrent(live, saved)
		if view.Resume != nil {
			t.Fatalf("stale restore facts retained: %+v", view.Resume)
		}
	}
}
