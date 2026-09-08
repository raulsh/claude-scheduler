// Package store owns the SQLite persistence layer.
//
// Two database handles are kept: a read pool for concurrent queries and a
// single-connection write handle. SQLite permits exactly one writer, so
// funnelling writes through one connection removes "database is locked"
// races rather than papering over them with retries.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// schemaVersion is bumped whenever schema.sql changes in a way that needs
// migrating. Version 1 is the initial schema.
const schemaVersion = 2

// Store provides access to the scheduler database.
type Store struct {
	read  *sql.DB
	write *sql.DB
	path  string
}

// Open prepares the database at path, creating parent directories and
// applying the schema if needed.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	// _txlock=immediate takes the write lock up front so a read-then-write
	// transaction cannot deadlock against another writer mid-transaction.
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"

	write, err := sql.Open("sqlite", dsn+"&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open write handle: %w", err)
	}
	write.SetMaxOpenConns(1)
	write.SetMaxIdleConns(1)
	write.SetConnMaxLifetime(0)

	read, err := sql.Open("sqlite", dsn)
	if err != nil {
		write.Close()
		return nil, fmt.Errorf("open read handle: %w", err)
	}
	read.SetMaxOpenConns(4)

	s := &Store{read: read, write: write, path: path}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.migrate(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// migrate applies the schema when the database is behind schemaVersion.
func (s *Store) migrate(ctx context.Context) error {
	if err := s.write.PingContext(ctx); err != nil {
		return fmt.Errorf("connect to %s: %w", s.path, err)
	}

	var current int
	if err := s.write.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if current >= schemaVersion {
		return nil
	}

	// schema.sql is written to be idempotent (IF NOT EXISTS throughout), so a
	// partially-created database converges rather than erroring.
	if _, err := s.write.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}

	// Column additions for databases created by an earlier version. CREATE
	// TABLE IF NOT EXISTS leaves an existing table untouched, so new columns
	// have to be added explicitly. A duplicate-column error means the
	// database is already current.
	for _, stmt := range []string{
		`ALTER TABLE tasks ADD COLUMN tools TEXT NOT NULL DEFAULT '[]'`,
	} {
		if _, err := s.write.ExecContext(ctx, stmt); err != nil && !isDuplicateColumn(err) {
			return fmt.Errorf("migrate: %s: %w", stmt, err)
		}
	}
	if _, err := s.write.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("set user_version: %w", err)
	}
	return nil
}

// Close releases both handles.
func (s *Store) Close() error {
	var firstErr error
	for _, db := range []*sql.DB{s.read, s.write} {
		if db == nil {
			continue
		}
		if err := db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Ping verifies the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.read.PingContext(ctx) }

// Path returns the database file location.
func (s *Store) Path() string { return s.path }

// timeFormat is the storage representation for all timestamps: UTC RFC3339
// with milliseconds, which sorts lexicographically.
const timeFormat = "2006-01-02T15:04:05.000Z"

func formatTime(t time.Time) string { return t.UTC().Format(timeFormat) }

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	// Accept both the millisecond form written here and plain RFC3339, since
	// SQLite defaults inserted by the schema use the same layout.
	for _, layout := range []string{timeFormat, time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp %q", s)
}

// nullTime converts a nullable column into a time, treating NULL as zero.
func nullTime(ns sql.NullString) (time.Time, error) {
	if !ns.Valid {
		return time.Time{}, nil
	}
	return parseTime(ns.String)
}

// timeArg renders a time for binding, mapping the zero time to NULL.
func timeArg(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return formatTime(t)
}

// isDuplicateColumn reports whether an ALTER TABLE failed only because the
// column is already present, which is the expected outcome on a database
// that the current schema already created.
func isDuplicateColumn(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
}
