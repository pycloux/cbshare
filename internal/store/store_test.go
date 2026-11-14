package store

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestDatabaseURI(t *testing.T) {
	q := url.Values{"vfs": {"adiantum"}}
	cases := map[string]string{
		// No "///" before the drive letter: Windows rejects "/C:/...".
		"C:/Users/runner/sub dir #1/cpsync.db": "file:C:/Users/runner/sub dir %231/cpsync.db?vfs=adiantum",
		"/home/a/100% ok?.db":                  "file:/home/a/100%25 ok%3F.db?vfs=adiantum",
	}
	for path, want := range cases {
		if got := databaseURI(path, q); got != want {
			t.Errorf("databaseURI(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestCreateReopenAndEncryption(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sub dir #1", "cpsync.db")

	s, err := Create(path, testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(ctx, "marker", []byte("PLAINTEXT-MARKER-VALUE")); err != nil {
		t.Fatal(err)
	}
	s.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(raw, []byte("SQLite format 3")) || bytes.Contains(raw, []byte("PLAINTEXT-MARKER-VALUE")) {
		t.Fatal("database file is not encrypted")
	}

	if _, err := Create(path, testKey(1)); !errors.Is(err, ErrExists) {
		t.Fatalf("Create on existing file: got %v", err)
	}
	if _, err := Open(path, testKey(2)); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("Open with wrong key: got %v", err)
	}

	s, err = Open(path, testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, ok, err := s.Setting(ctx, "marker")
	if err != nil || !ok || string(v) != "PLAINTEXT-MARKER-VALUE" {
		t.Fatalf("Setting = %q, %v, %v", v, ok, err)
	}
	if _, ok, _ := s.Setting(ctx, "missing"); ok {
		t.Fatal("missing setting reported as present")
	}
	if err := s.SetSetting(ctx, "marker", []byte("new")); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := s.Setting(ctx, "marker"); string(v) != "new" {
		t.Fatalf("overwrite failed: %q", v)
	}
}

func TestOpenMissing(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "nope.db"), testKey(1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.db")
	s, err := Create(path, testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	for _, side := range []string{"-journal", "-wal"} {
		if err := os.WriteFile(path+side, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Remove(path); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("files left behind: %v", left)
	}
	if err := Remove(path); err != nil {
		t.Fatalf("removing a missing database must not fail: %v", err)
	}
}
