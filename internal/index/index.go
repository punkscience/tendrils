// Package index is the device's local memory of the last state it synced for
// every path — the "base" that reconciliation compares against. It is a small
// embedded bbolt store (pure Go, no CGO) so it survives reboots and lets a
// device that was off for a week pick up exactly where it left off.
package index

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"go.etcd.io/bbolt"

	"ca.punkscience.tendrils/internal/tree"
)

var (
	entriesBucket = []byte("entries")
	metaBucket    = []byte("meta")
	lastReconcile = []byte("last-reconcile")
)

// Store is the persistent index. Safe for use from the single sync engine.
type Store struct {
	db *bbolt.DB
}

// Open opens (creating if needed) the index database at path.
func Open(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("index: open %s: %w", path, err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, b := range [][]byte{entriesBucket, metaBucket} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("index: init buckets: %w", err)
	}
	return &Store{db: db}, nil
}

// OpenReadOnly opens an existing index database at path in read-only mode,
// which does not acquire an exclusive lock and therefore succeeds even when
// the daemon has the database open. If path does not exist (no syncs have
// run yet), it returns (nil, nil); callers must check for a nil *Store.
func OpenReadOnly(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{
		ReadOnly: true,
		Timeout:  1 * time.Second,
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("index: open %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close releases the database file.
func (s *Store) Close() error { return s.db.Close() }

// Put records the last-synced entry for its path, overwriting any prior one.
func (s *Store) Put(e *tree.Entry) error {
	if e == nil || e.Path == "" {
		return fmt.Errorf("index: put: entry has no path")
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("index: marshal %s: %w", e.Path, err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(entriesBucket).Put([]byte(e.Path), data)
	})
}

// Get returns the recorded entry for path, or nil if none is recorded.
func (s *Store) Get(path string) (*tree.Entry, error) {
	var e *tree.Entry
	err := s.db.View(func(tx *bbolt.Tx) error {
		data := tx.Bucket(entriesBucket).Get([]byte(path))
		if data == nil {
			return nil
		}
		e = &tree.Entry{}
		return json.Unmarshal(data, e)
	})
	if err != nil {
		return nil, fmt.Errorf("index: get %s: %w", path, err)
	}
	return e, nil
}

// All returns every recorded entry keyed by path, tombstones included. The
// reconciler needs tombstones to tell a re-creation from a fresh file.
func (s *Store) All() (map[string]*tree.Entry, error) {
	out := make(map[string]*tree.Entry)
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(entriesBucket).ForEach(func(k, v []byte) error {
			e := &tree.Entry{}
			if err := json.Unmarshal(v, e); err != nil {
				return err
			}
			out[string(k)] = e
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("index: all: %w", err)
	}
	return out, nil
}

// SetLastReconcile records the time of the last successful reconcile, reported
// by the status command.
func (s *Store) SetLastReconcile(at time.Time) error {
	b, _ := at.MarshalBinary()
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(metaBucket).Put(lastReconcile, b)
	})
}

// LastReconcile returns the recorded last-reconcile time; the zero time if none.
func (s *Store) LastReconcile() (time.Time, error) {
	var at time.Time
	err := s.db.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(metaBucket).Get(lastReconcile)
		if v == nil {
			return nil
		}
		return at.UnmarshalBinary(v)
	})
	return at, err
}
