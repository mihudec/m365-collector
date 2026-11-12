package collector

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	_ "modernc.org/sqlite"
)

type State struct {
	db   *sql.DB
	lock *flock.Flock
}

func OpenState(path string) (*State, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	l := flock.New(path + ".lock")
	ok, err := l.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("another collector holds the state lock")
	}
	fail := func(e error) (*State, error) { _ = l.Unlock(); return nil, e }
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fail(err)
	}
	_ = f.Close()
	if err = os.Chmod(path, 0600); err != nil {
		return fail(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return fail(err)
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=DELETE; PRAGMA synchronous=FULL;
 CREATE TABLE IF NOT EXISTS cursors (scope TEXT PRIMARY KEY, stamp INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS completed (scope TEXT NOT NULL, hash TEXT NOT NULL, stamp INTEGER NOT NULL, PRIMARY KEY(scope,hash));`)
	if err != nil {
		_ = db.Close()
		return fail(err)
	}
	return &State{db, l}, nil
}
func (s *State) Close() error {
	e := s.db.Close()
	e2 := s.lock.Unlock()
	if e != nil {
		return e
	}
	return e2
}
func scope(tenant, kind string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(tenant+":"+kind)))
}
func (s *State) cursor(key string) (time.Time, error) {
	var n int64
	e := s.db.QueryRow("SELECT stamp FROM cursors WHERE scope=?", key).Scan(&n)
	if e == sql.ErrNoRows {
		return time.Time{}, nil
	}
	return time.Unix(n, 0).UTC(), e
}
func (s *State) advance(key string, t time.Time) error {
	_, e := s.db.Exec("INSERT INTO cursors VALUES (?,?) ON CONFLICT(scope) DO UPDATE SET stamp=max(stamp,excluded.stamp)", key, t.Unix())
	return e
}
func blobHash(id string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(id))) }
func (s *State) done(key, id string) (bool, error) {
	var n int
	e := s.db.QueryRow("SELECT count(*) FROM completed WHERE scope=? AND hash=?", key, blobHash(id)).Scan(&n)
	return n > 0, e
}
func (s *State) mark(key, id string, t time.Time) error {
	_, e := s.db.Exec("INSERT OR REPLACE INTO completed VALUES (?,?,?)", key, blobHash(id), t.Unix())
	return e
}
func (s *State) prune(now time.Time) error {
	_, e := s.db.Exec("DELETE FROM completed WHERE stamp < ?", now.Add(-31*24*time.Hour).Unix())
	return e
}
