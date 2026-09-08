package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// KVEntry is one key's metadata. The value is carried separately because
// listing a namespace should not pull every byte of every value with it.
type KVEntry struct {
	Key       string    `json:"key"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`

	// Value is set by GetKV and left empty by ListKV.
	Value []byte `json:"-"`
}

// Expires reports whether the entry has a TTL.
func (e KVEntry) Expires() bool { return !e.ExpiresAt.IsZero() }

// kvMetaColumns is the metadata projection shared by Get and List. length()
// is computed in SQLite so a listing never transfers the values themselves.
const kvMetaColumns = `key, length(value), created_at, updated_at, expires_at`

// unexpired filters out entries whose TTL has passed. Comparing the stored
// TEXT timestamps directly is sound because timeFormat is fixed-width UTC
// and therefore sorts lexicographically.
const unexpired = ` AND (expires_at IS NULL OR expires_at > ?)`

func scanKVMeta(sc interface{ Scan(...any) error }) (KVEntry, error) {
	var (
		e         KVEntry
		createdAt string
		updatedAt string
		expiresAt sql.NullString
	)
	if err := sc.Scan(&e.Key, &e.Size, &createdAt, &updatedAt, &expiresAt); err != nil {
		return e, err
	}

	var err error
	if e.CreatedAt, err = parseTime(createdAt); err != nil {
		return e, err
	}
	if e.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return e, err
	}
	if e.ExpiresAt, err = nullTime(expiresAt); err != nil {
		return e, err
	}
	return e, nil
}

// SetKV writes a value, replacing whatever the key held.
//
// A zero expiresAt clears any TTL the key already had. "Set this value" is
// the plainest reading of the command, and a value that silently kept an
// expiry inherited from a previous write would be a trap.
func (s *Store) SetKV(ctx context.Context, key string, value []byte, expiresAt time.Time) error {
	if key == "" {
		return errors.New("key is empty")
	}
	// A nil slice binds as NULL, which the NOT NULL column rejects. Storing
	// an empty value is legitimate, so normalise here rather than failing in
	// the driver several layers down.
	if value == nil {
		value = []byte{}
	}

	now := formatTime(time.Now())
	_, err := s.write.ExecContext(ctx, `
		INSERT INTO kv (key, value, created_at, updated_at, expires_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET
			value      = excluded.value,
			updated_at = excluded.updated_at,
			expires_at = excluded.expires_at`,
		key, value, now, now, timeArg(expiresAt))
	if err != nil {
		return fmt.Errorf("set kv %q: %w", key, err)
	}
	return nil
}

// GetKV reads one value. An expired key reports ErrNotFound: a value whose
// TTL has passed is gone as far as any caller is concerned, whether or not
// the row has been swept yet.
func (s *Store) GetKV(ctx context.Context, key string) (*KVEntry, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+kvMetaColumns+`, value FROM kv WHERE key = ?`+unexpired,
		key, formatTime(time.Now()))

	var (
		e         KVEntry
		createdAt string
		updatedAt string
		expiresAt sql.NullString
	)
	err := row.Scan(&e.Key, &e.Size, &createdAt, &updatedAt, &expiresAt, &e.Value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get kv %q: %w", key, err)
	}

	if e.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if e.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	if e.ExpiresAt, err = nullTime(expiresAt); err != nil {
		return nil, err
	}
	// The driver returns a zero-length blob as a nil slice. Callers should
	// not have to tell "empty" from "absent" when the store already has.
	if e.Value == nil {
		e.Value = []byte{}
	}
	return &e, nil
}

// ListKV returns metadata for every live key, optionally restricted to a
// prefix.
func (s *Store) ListKV(ctx context.Context, prefix string) ([]KVEntry, error) {
	query := `SELECT ` + kvMetaColumns + ` FROM kv WHERE 1=1`
	args := []any{}

	if prefix != "" {
		// A range scan rather than LIKE: LIKE would treat % and _ inside the
		// prefix as wildcards, and could not use the primary key index.
		query += ` AND key >= ?`
		args = append(args, prefix)
		if upper, ok := prefixUpperBound(prefix); ok {
			query += ` AND key < ?`
			args = append(args, upper)
		}
	}
	query += unexpired + ` ORDER BY key`
	args = append(args, formatTime(time.Now()))

	rows, err := s.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list kv: %w", err)
	}
	defer rows.Close()

	var entries []KVEntry
	for rows.Next() {
		e, err := scanKVMeta(rows)
		if err != nil {
			return nil, fmt.Errorf("scan kv: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// DeleteKV removes a key.
func (s *Store) DeleteKV(ctx context.Context, key string) error {
	res, err := s.write.ExecContext(ctx, `DELETE FROM kv WHERE key = ?`, key)
	if err != nil {
		return fmt.Errorf("delete kv %q: %w", key, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PurgeExpiredKV removes rows whose TTL has passed. Reads already hide them,
// so this only reclaims space; the daemon runs it at startup.
func (s *Store) PurgeExpiredKV(ctx context.Context) (int64, error) {
	res, err := s.write.ExecContext(ctx,
		`DELETE FROM kv WHERE expires_at IS NOT NULL AND expires_at <= ?`,
		formatTime(time.Now()))
	if err != nil {
		return 0, fmt.Errorf("purge expired kv: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// prefixUpperBound returns the first key that sorts after every key
// beginning with prefix, so a prefix match becomes a half-open range.
//
// Incrementing the last byte is what makes "job/" cover "job/a" and "job/z"
// but not "joc". A trailing 0xff carries into the byte before it. ok is
// false when every byte is 0xff, because then no key sorts after the prefix
// and the lower bound alone is the correct filter.
func prefixUpperBound(prefix string) (string, bool) {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1]), true
		}
	}
	return "", false
}
