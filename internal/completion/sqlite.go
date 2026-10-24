// Package completion persists disposable piece hints in the application's
// SQLite database. Get/Set never perform disk I/O on a library callback thread.
package completion

import (
	"container/list"
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

const CacheEntries = 4096
const PendingEntries = 1024

type entry struct {
	key      metainfo.PieceKey
	complete bool
}
type Stats struct {
	Cached, Pending int
	Dropped         uint64
	Error           string
}
type SQLite struct {
	db      *sql.DB
	mu      sync.Mutex
	cache   map[metainfo.PieceKey]*list.Element
	lru     list.List
	pending map[metainfo.PieceKey]bool
	dropped uint64
	lastErr error
	closed  bool
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
}

func New(db *sql.DB) (*SQLite, error) {
	// Probe at construction, outside library locks. Hints are loaded only into
	// a bounded cache and remain untrusted by VerifyOnOpen storage.
	rows, err := db.Query(`SELECT infohash,piece,complete FROM piece_completion LIMIT ?`, CacheEntries)
	if err != nil {
		return nil, err
	}
	s := &SQLite{db: db, cache: make(map[metainfo.PieceKey]*list.Element), pending: make(map[metainfo.PieceKey]bool), wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	for rows.Next() {
		var raw []byte
		var key metainfo.PieceKey
		var complete bool
		if err = rows.Scan(&raw, &key.Index, &complete); err != nil {
			rows.Close()
			return nil, err
		}
		if len(raw) != len(key.InfoHash) || key.Index < 0 {
			continue
		}
		copy(key.InfoHash[:], raw)
		s.put(key, complete)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	go s.run()
	return s, nil
}

func (s *SQLite) put(key metainfo.PieceKey, complete bool) {
	if e := s.cache[key]; e != nil {
		e.Value = entry{key, complete}
		s.lru.MoveToFront(e)
		return
	}
	if len(s.cache) == CacheEntries {
		old := s.lru.Back()
		delete(s.cache, old.Value.(entry).key)
		s.lru.Remove(old)
	}
	s.cache[key] = s.lru.PushFront(entry{key, complete})
}
func (s *SQLite) Get(key metainfo.PieceKey) (storage.Completion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return storage.Completion{}, sql.ErrConnDone
	}
	if e := s.cache[key]; e != nil {
		s.lru.MoveToFront(e)
		return storage.Completion{Ok: true, Complete: e.Value.(entry).complete}, nil
	}
	return storage.Completion{}, nil
}
func (s *SQLite) Set(key metainfo.PieceKey, complete bool) error {
	if key.Index < 0 {
		return errors.New("invalid piece index")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return sql.ErrConnDone
	}
	s.put(key, complete)
	if _, ok := s.pending[key]; !ok && len(s.pending) == PendingEntries {
		s.dropped++
		return nil
	}
	s.pending[key] = complete
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}
func (s *SQLite) Persistent() bool { return true }
func (s *SQLite) Status() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := Stats{Cached: len(s.cache), Pending: len(s.pending), Dropped: s.dropped}
	if s.lastErr != nil {
		v.Error = "completion persistence failed"
	}
	return v
}
func (s *SQLite) flush(ctx context.Context) error {
	s.mu.Lock()
	batch := s.pending
	s.pending = make(map[metainfo.PieceKey]bool)
	s.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err == nil {
		defer tx.Rollback()
		var stmt *sql.Stmt
		stmt, err = tx.PrepareContext(ctx, `INSERT INTO piece_completion(infohash,piece,complete) VALUES(?,?,?) ON CONFLICT(infohash,piece) DO UPDATE SET complete=excluded.complete`)
		if err == nil {
			defer stmt.Close()
			for key, v := range batch {
				if _, err = stmt.ExecContext(ctx, key.InfoHash[:], key.Index, v); err != nil {
					break
				}
			}
		}
		if err == nil {
			err = tx.Commit()
		}
	}
	if err != nil {
		s.mu.Lock()
		s.lastErr = err
		s.dropped += uint64(len(batch))
		s.mu.Unlock()
	}
	return err
}
func (s *SQLite) run() {
	defer close(s.done)
	for {
		select {
		case <-s.wake:
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			s.flush(ctx)
			cancel()
		case <-s.stop:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			s.flush(ctx)
			cancel()
			return
		}
	}
}
func (s *SQLite) Close() error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.stop)
	}
	s.mu.Unlock()
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}
