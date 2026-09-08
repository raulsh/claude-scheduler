package store

import (
	"context"
	"fmt"
)

// AppendEvent records one parsed NDJSON line of an execution's transcript.
// Duplicate sequence numbers are ignored so a replayed tail is harmless.
func (s *Store) AppendEvent(ctx context.Context, e *ExecutionEvent) error {
	res, err := s.write.ExecContext(ctx, `
		INSERT OR IGNORE INTO execution_events (execution_id, seq, ts, type, subtype, payload)
		VALUES (?,?,?,?,?,?)`,
		e.ExecutionID, e.Seq, formatTime(nonZeroTime(e.TS)), e.Type, e.Subtype, string(e.Payload))
	if err != nil {
		return fmt.Errorf("append event %d/%d: %w", e.ExecutionID, e.Seq, err)
	}
	if id, err := res.LastInsertId(); err == nil {
		e.ID = id
	}
	return nil
}

// ListEvents returns an execution's transcript in order, starting after
// afterSeq. A limit of zero applies a sane default.
func (s *Store) ListEvents(ctx context.Context, executionID, afterSeq int64, limit int) ([]ExecutionEvent, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	rows, err := s.read.QueryContext(ctx, `
		SELECT id, execution_id, seq, ts, type, subtype, payload
		FROM execution_events
		WHERE execution_id=? AND seq > ?
		ORDER BY seq LIMIT ?`, executionID, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("list events for %d: %w", executionID, err)
	}
	defer rows.Close()

	var out []ExecutionEvent
	for rows.Next() {
		var (
			e       ExecutionEvent
			ts      string
			payload string
		)
		if err := rows.Scan(&e.ID, &e.ExecutionID, &e.Seq, &ts, &e.Type, &e.Subtype, &payload); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		if e.TS, err = parseTime(ts); err != nil {
			return nil, err
		}
		e.Payload = []byte(payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

// MaxEventSeq reports the highest sequence number stored for an execution,
// which is what an SSE reconnect resumes from.
func (s *Store) MaxEventSeq(ctx context.Context, executionID int64) (int64, error) {
	var seq *int64
	err := s.read.QueryRowContext(ctx,
		`SELECT MAX(seq) FROM execution_events WHERE execution_id=?`, executionID).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("max seq for %d: %w", executionID, err)
	}
	if seq == nil {
		return 0, nil
	}
	return *seq, nil
}
