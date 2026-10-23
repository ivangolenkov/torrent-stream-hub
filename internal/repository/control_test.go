package repository

import (
	"encoding/json"
	"strings"
	"testing"
	"torrent-stream-hub/internal/models"
)

func TestControlReadSnapshotDoesNotMixConcurrentCommand(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	r := NewTorrentRepo(db)
	i := intentFixture(t, r, "snapshot-control")
	tx, err := db.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var oldRevision int64
	if err := tx.QueryRow(`SELECT revision FROM torrent_intents WHERE hash=?`, i.Version.Hash).Scan(&oldRevision); err != nil {
		t.Fatal(err)
	}
	// The write commits after the read snapshot starts, without blocking WAL.
	if err := r.UpdateFilePriority(i.Version.Hash, 0, models.PriorityNone); err != nil {
		t.Fatal(err)
	}
	tor := &models.Torrent{Hash: i.Version.Hash}
	if err := r.loadFiles(tx, tor); err != nil {
		t.Fatal(err)
	}
	if err := loadControls(tx, map[string]*models.Torrent{tor.Hash: tor}); err != nil {
		t.Fatal(err)
	}
	if tor.Control.DesiredRevision != oldRevision || tor.Files[0].Priority == models.PriorityNone {
		t.Fatal("mixed old intent with new selection", tor)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	now, err := r.GetTorrent(tor.Hash)
	if err != nil || now.Control.DesiredRevision != oldRevision+1 || now.Files[0].Priority != models.PriorityNone {
		t.Fatal(now, err)
	}
}

func TestControlRevisionsSerializeWithoutFloatPrecisionLoss(t *testing.T) {
	c := models.TorrentControl{DesiredRevision: 9223372036854775807, AppliedRevision: 9007199254740993}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"desired_revision":"9223372036854775807"`) || !strings.Contains(string(b), `"applied_revision":"9007199254740993"`) {
		t.Fatal(string(b))
	}
}
