// Package store keeps what telesfor must remember across restarts, such as a
// provider's sign-in, in one bbolt file. Each owner has a bucket; bbolt allows
// only one handle per file.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

const File = "telesfor.db"

// Open rejects database versions newer than Version.
const Version = 1

var (
	metaBucket = []byte("meta")
	versionKey = []byte("version")
)

// Open opens the database in dir, creating both if needed. They are readable
// by their owner only: the database holds sign-ins.
func Open(dir string) (*bolt.DB, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, File)
	// Bound the wait for another process's file lock.
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if errors.Is(err, bolterrors.ErrTimeout) {
		return nil, fmt.Errorf("%s is in use by another telesfor", path)
	}
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	if err := db.Update(checkVersion); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return db, nil
}

func checkVersion(tx *bolt.Tx) error {
	b, err := tx.CreateBucketIfNotExists(metaBucket)
	if err != nil {
		return err
	}
	v := b.Get(versionKey)
	if v == nil {
		return b.Put(versionKey, []byte(strconv.Itoa(Version)))
	}
	n, err := strconv.Atoi(string(v))
	if err != nil {
		return fmt.Errorf("unknown format version %q", v)
	}
	if n > Version {
		return fmt.Errorf("format version %d is from a newer telesfor, which this one (%d) can't read", n, Version)
	}
	return nil
}

// Get decodes what is kept under key in owner's bucket, as JSON, into v. It
// reports whether anything is kept there: a nil db keeps nothing.
func Get(db *bolt.DB, owner, key string, v any) (bool, error) {
	if db == nil {
		return false, nil
	}
	var found bool
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(owner))
		if b == nil {
			return nil
		}
		data := b.Get([]byte(key))
		if data == nil {
			return nil
		}
		found = true
		return json.Unmarshal(data, v) // data is valid only within the transaction
	})
	if err != nil {
		return false, fmt.Errorf("reading %s from %s: %w", key, db.Path(), err)
	}
	return found, nil
}

// Put keeps v under key in owner's bucket, as JSON. A nil db keeps nothing.
func Put(db *bolt.DB, owner, key string, v any) error {
	if db == nil {
		return nil
	}
	err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(owner))
		if err != nil {
			return err
		}
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return b.Put([]byte(key), data)
	})
	if err != nil {
		return fmt.Errorf("writing %s to %s: %w", key, db.Path(), err)
	}
	return nil
}

// Delete removes what is kept under key in owner's bucket, if anything is.
func Delete(db *bolt.DB, owner, key string) error {
	if db == nil {
		return nil
	}
	err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(owner))
		if b == nil {
			return nil
		}
		return b.Delete([]byte(key))
	})
	if err != nil {
		return fmt.Errorf("removing %s from %s: %w", key, db.Path(), err)
	}
	return nil
}
