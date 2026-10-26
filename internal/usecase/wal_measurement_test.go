package usecase

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"torrent-stream-hub/internal/repository"
)

type measurementWAL struct {
	Salt           [8]byte
	Commits, Bytes int64
}

func sampleMeasurementDatabaseWAL(db *repository.SQLiteDB, path string) (measurementWAL, error) {
	// Hash queues settling does not join the disposable hint writer. Reserve
	// SQLite's writer while sampling so its async commit cannot append a partial
	// frame between Stat and ReadAt. This empty transaction writes no WAL frame.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tx, err := db.BeginWrite(ctx)
	if err != nil {
		return measurementWAL{}, err
	}
	defer tx.Rollback()
	return sampleMeasurementWAL(path)
}

// Test/benchmark-only parser of SQLite WAL framing. Call with quiesced writers
// and autocheckpoint disabled; a nonzero frame database-size field ends a write
// transaction. Empty/read transactions do not write commit frames and are not
// counted. No claim about actual OS fsync calls is inferred from these frames.
func sampleMeasurementWAL(path string) (measurementWAL, error) {
	var out measurementWAL
	f, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer f.Close()
	var header [32]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return out, err
	}
	magic := binary.BigEndian.Uint32(header[:4])
	if magic != 0x377f0682 && magic != 0x377f0683 {
		return out, fmt.Errorf("not a SQLite WAL")
	}
	pageSize := int64(binary.BigEndian.Uint32(header[8:12]))
	if pageSize < 512 || pageSize > 65536 || pageSize&(pageSize-1) != 0 {
		return out, fmt.Errorf("bad WAL page size")
	}
	copy(out.Salt[:], header[16:24])
	st, err := f.Stat()
	if err != nil {
		return out, err
	}
	if (st.Size()-32)%(24+pageSize) != 0 {
		return out, fmt.Errorf("partial WAL frame")
	}
	out.Bytes = st.Size()
	var frame [24]byte
	for off := int64(32); off < st.Size(); off += 24 + pageSize {
		if _, err := f.ReadAt(frame[:], off); err != nil {
			return out, err
		}
		if string(frame[8:16]) != string(out.Salt[:]) {
			return out, fmt.Errorf("stale WAL frame")
		}
		if binary.BigEndian.Uint32(frame[4:8]) != 0 {
			out.Commits++
		}
	}
	return out, nil
}

func TestResumeMeasurementWALCountsTransactionsNotRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "measure.db")
	db, err := repository.NewSQLiteDB(path + "?_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB().Exec(`CREATE TABLE measured(n INTEGER)`); err != nil {
		t.Fatal(err)
	}
	before, err := sampleMeasurementDatabaseWAL(db, path+"-wal")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 17; i++ {
		if _, err := tx.Exec(`INSERT INTO measured VALUES(?)`, i); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`INSERT INTO measured VALUES(18)`); err != nil {
		t.Fatal(err)
	}
	tx, err = db.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO measured VALUES(19)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	after, err := sampleMeasurementDatabaseWAL(db, path+"-wal")
	if err != nil {
		t.Fatal(err)
	}
	if before.Salt != after.Salt || after.Commits-before.Commits != 2 {
		t.Fatalf("commits before=%+v after=%+v", before, after)
	}
}
