// Package store keeps cbshare's data in an encrypted SQLite database.
//
// Encryption uses the Adiantum VFS of github.com/ncruces/go-sqlite3: every
// page of the database and its journals is encrypted with the key, so the
// file cannot be read with an ordinary SQLite client.
package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/ncruces/go-sqlite3/driver"       // database/sql driver "sqlite3"
	_ "github.com/ncruces/go-sqlite3/embed"        // embedded SQLite build
	_ "github.com/ncruces/go-sqlite3/vfs/adiantum" // encrypting VFS
)

var (
	// ErrExists is returned by Create when the database file already exists.
	ErrExists = errors.New("database already exists")
	// ErrNotFound is returned by Open when the database file is missing.
	ErrNotFound = errors.New("database not found")
	// ErrWrongKey is returned by Open when the key does not decrypt the database.
	ErrWrongKey = errors.New("wrong key: the database cannot be decrypted with it")
	// ErrOldSchema is returned by Open for a database in the old format
	// (schema 1), which had one authenticator code shared by everyone
	// instead of one per user. Only cpsync v0.2, the project cbshare grew
	// out of, wrote that format; no cbshare release did. There is no
	// migration: the database must be recreated.
	ErrOldSchema = errors.New("this database uses an old format with one authenticator code shared by everyone, " +
		"which this version cannot open: delete it and start again with --password")
)

// Setting keys. The settings table is a small key/value store for the
// server's own secrets; package tlsutil adds its certificate there too.
// Authenticator secrets live in the users table, one per user.
const SettingAdminHash = "admin_password_hash"

// schemaVersion is stored in SQLite's user_version pragma.
//
//	1  one shared authenticator secret in settings (cpsync v0.2 only)
//	2  a users table with one secret per user (every cbshare release)
//
// Version 1 databases are refused (ErrOldSchema); see migrate.
const schemaVersion = 2

// schema creates the tables. Files live in their own table, one per
// message at most, and disappear with their message (ON DELETE CASCADE,
// which needs the foreign_keys pragma set in open). Deleting a user keeps
// their messages: user_id becomes NULL (ON DELETE SET NULL) while author
// keeps the name they were posted under.
const schema = `
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS users (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	name             TEXT    NOT NULL,
	name_key         TEXT    NOT NULL UNIQUE,   -- lower-cased name: names are unique whatever the case
	is_admin         INTEGER NOT NULL DEFAULT 0,
	status           TEXT    NOT NULL CHECK (status IN ('pending', 'active', 'revoked', 'expired')),
	totp_secret      TEXT,                      -- NULL once revoked or expired
	enrol_expires_at INTEGER,                   -- unix milliseconds, while pending
	created_at       INTEGER NOT NULL,
	activated_at     INTEGER,
	last_login_at    INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS users_single_admin ON users(is_admin) WHERE is_admin = 1;
CREATE TABLE IF NOT EXISTS messages (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	kind       TEXT    NOT NULL,          -- text | file | system
	user_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,  -- NULL: system message or deleted user
	author     TEXT    NOT NULL,
	body       TEXT    NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,          -- unix milliseconds
	edited_at  INTEGER                    -- unix milliseconds, NULL if never edited
);
CREATE TABLE IF NOT EXISTS files (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	message_id INTEGER NOT NULL UNIQUE REFERENCES messages(id) ON DELETE CASCADE,
	name       TEXT    NOT NULL,
	mime       TEXT    NOT NULL,
	size       INTEGER NOT NULL,
	data       BLOB    NOT NULL
);
`

// Store is an open, decrypted database.
type Store struct {
	db *sql.DB
}

// Create creates a new encrypted database at path. It fails if the file exists.
func Create(path string, key []byte) (*Store, error) {
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("%w: %s (restart with --key, or remove the file to start over)", ErrExists, path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	s, err := open(path, key)
	if err != nil {
		Remove(path)
		return nil, err
	}
	if err := s.migrate(); err != nil {
		s.Close()
		Remove(path)
		return nil, err
	}
	return s, nil
}

// Remove deletes a database file together with the side files SQLite may
// have left next to it (rollback journal, write-ahead log). It is used to
// clean up a database whose creation did not complete; files that do not
// exist are ignored.
func Remove(path string) error {
	var errs []error
	for _, p := range []string{path, path + "-journal", path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Open opens an existing encrypted database at path with key.
func Open(path string, key []byte) (*Store, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s (start with --password to create it)", ErrNotFound, path)
	}
	s, err := open(path, key)
	if err != nil {
		return nil, err
	}
	if err := s.migrate(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func open(path string, key []byte) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// Connection options, applied in order by the driver:
	//   vfs=adiantum      encrypt every page of the file and its journal
	//   hexkey            the 256-bit key (must come first)
	//   foreign_keys      enable ON DELETE CASCADE for files
	//   busy_timeout      wait instead of failing if the file is locked
	//   secure_delete     overwrite deleted content inside the file
	q := url.Values{}
	q.Set("vfs", "adiantum")
	q.Add("_pragma", "hexkey('"+hex.EncodeToString(key)+"')")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "secure_delete(1)")

	db, err := sql.Open("sqlite3", databaseURI(abs, q))
	if err != nil {
		return nil, err
	}
	// A single connection serialises access; the data set is tiny.
	db.SetMaxOpenConns(1)
	db.SetConnMaxIdleTime(0)
	db.SetConnMaxLifetime(0)

	// The key is only checked when SQLite first reads the file: run a query
	// now so that a wrong key is reported at startup with a clear message.
	var n int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master").Scan(&n); err != nil {
		db.Close()
		if strings.Contains(err.Error(), "not a database") {
			return nil, ErrWrongKey
		}
		return nil, fmt.Errorf("open database: %w", err)
	}
	return &Store{db: db}, nil
}

// databaseURI builds the SQLite URI for an absolute path, without an
// authority: "file:/home/x.db" on Linux, "file:C:/x.db" on Windows.
//
// The "file:///C:/x.db" form must not be used: the driver's Go file layer
// receives "/C:/x.db", which Windows rejects ("CreateFile /C:: The
// filename, directory name, or volume label syntax is incorrect").
// Only the characters with a meaning in a URI need escaping.
func databaseURI(path string, query url.Values) string {
	p := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(filepath.ToSlash(path))
	return "file:" + p + "?" + query.Encode()
}

// migrate creates the schema if needed. It refuses databases created by a
// newer cbshare, whose schema this version might damage, and those in the
// old shared-code format (schema 1), which are not migrated.
func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v == 1 {
		return ErrOldSchema
	}
	if v > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than this cbshare supports (%d)", v, schemaVersion)
	}
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	_, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion))
	return err
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Setting returns the value of a setting, or ok=false when it is not set.
func (s *Store) Setting(ctx context.Context, key string) (value []byte, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return value, err == nil, err
}

// SetSetting stores the value of a setting.
func (s *Store) SetSetting(ctx context.Context, key string, value []byte) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		key, value)
	return err
}
