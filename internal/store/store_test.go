package store

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestOpenCreatesAPrivateDatabase(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var version string
	db.View(func(tx *bolt.Tx) error {
		version = string(tx.Bucket(metaBucket).Get(versionKey))
		return nil
	})
	if version != "1" {
		t.Errorf("version = %q, want 1", version)
	}
	if runtime.GOOS == "windows" {
		return // no permission bits to check
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, File): 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %o, want %o", path, got, want)
		}
	}
}

func TestNewerVersionIsRefused(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(metaBucket).Put(versionKey, []byte("2"))
	})
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if db, err := Open(dir); err == nil || !strings.Contains(err.Error(), "newer telesfor") {
		if db != nil {
			db.Close()
		}
		t.Fatalf("err = %v, want a newer version refused", err)
	}
}

func TestDatabaseInUse(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "in use by another telesfor") {
		t.Fatalf("err = %v", err)
	}
}
