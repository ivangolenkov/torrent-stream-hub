package repository

import (
	"fmt"
	"path/filepath"
	"testing"

	"torrent-stream-hub/internal/models"
)

func BenchmarkUnchangedObservation(b *testing.B) {
	db, err := NewSQLiteDB(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	r := NewTorrentRepo(db)
	c := models.TorrentCatalog{Format: "v1", Name: "bench", Size: 1000, PieceLength: 1, NumPieces: 1000}
	for i := range 1000 {
		c.Files = append(c.Files, models.FileCatalog{Index: i, Path: fmt.Sprint(i), Size: 1, Offset: int64(i)})
	}
	if err := r.CreateTorrent(models.NewTorrentRecord{Hash: "bench", Name: "bench", Run: models.RunEnabled, Catalog: &c}); err != nil {
		b.Fatal(err)
	}
	i, err := r.GetIntent("bench")
	if err != nil {
		b.Fatal(err)
	}
	f, err := r.AttachRuntime(i.Version)
	if err != nil {
		b.Fatal(err)
	}
	o := models.RuntimeObservation{Fence: f, AppliedRevision: f.Revision, State: models.StateQueued, ProgressSource: "verified", ObservedAt: 1}
	for i := range 1000 {
		o.Files = append(o.Files, models.FileObservation{Index: i})
	}
	if _, err := db.DB().Exec(`CREATE TABLE write_count(n INTEGER); INSERT INTO write_count VALUES(0); CREATE TRIGGER count_file_writes AFTER UPDATE ON files BEGIN UPDATE write_count SET n=n+1; END;`); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := range b.N {
		o.Sequence = int64(n + 1)
		o.ObservedAt = int64(n + 1)
		if err := r.SaveCatalog(f, c); err != nil {
			b.Fatal(err)
		}
		if err := r.SaveRuntimeObservation(o); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	var count int64
	if err := db.DB().QueryRow(`SELECT n FROM write_count`).Scan(&count); err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(count)/float64(b.N), "file-writes/op")
}
