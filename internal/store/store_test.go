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

func TestGetPutDelete(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	type value struct{ Name string }
	var got value
	if found, err := Get(db, "owner", "key", &got); found || err != nil {
		t.Fatalf("before Put: found = %v, err = %v", found, err)
	}
	if err := Put(db, "owner", "key", value{"one"}); err != nil {
		t.Fatal(err)
	}
	if found, err := Get(db, "owner", "key", &got); !found || err != nil || got.Name != "one" {
		t.Fatalf("after Put: found = %v, err = %v, got %+v", found, err, got)
	}
	// It is kept as JSON, under the key, in a bucket named for the owner.
	var kept string
	db.View(func(tx *bolt.Tx) error {
		kept = string(tx.Bucket([]byte("owner")).Get([]byte("key")))
		return nil
	})
	if kept != `{"Name":"one"}` {
		t.Errorf("kept %q", kept)
	}
	if found, _ := Get(db, "other", "key", &got); found {
		t.Error("found the key in another owner's bucket")
	}
	if found, _ := Get(db, "owner", "other", &got); found {
		t.Error("found a key never kept")
	}
	if err := Delete(db, "owner", "key"); err != nil {
		t.Fatal(err)
	}
	if found, err := Get(db, "owner", "key", &got); found || err != nil {
		t.Errorf("after Delete: found = %v, err = %v", found, err)
	}
	if err := Delete(db, "other", "key"); err != nil {
		t.Errorf("deleting what was never kept: %v", err)
	}
}

func TestNilDatabaseKeepsNothing(t *testing.T) {
	if err := Put(nil, "owner", "key", 1); err != nil {
		t.Fatal(err)
	}
	var n int
	if found, err := Get(nil, "owner", "key", &n); found || err != nil {
		t.Errorf("found = %v, err = %v", found, err)
	}
	if err := Delete(nil, "owner", "key"); err != nil {
		t.Error(err)
	}
}

func TestGetNamesWhatItCannotRead(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Put(db, "owner", "key", "words"); err != nil {
		t.Fatal(err)
	}
	var n int
	found, err := Get(db, "owner", "key", &n)
	if want := "reading key from " + db.Path(); found || err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("found = %v, err = %v, want an error naming %q", found, err, want)
	}
}
