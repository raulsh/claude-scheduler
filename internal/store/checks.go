package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SaveCheckResult records a healthcheck outcome, optionally tied to the
// execution it gated.
func (s *Store) SaveCheckResult(ctx context.Context, c *CheckResult) error {
	if c.CheckedAt.IsZero() {
		c.CheckedAt = time.Now()
	}
	res, err := s.write.ExecContext(ctx, `
		INSERT INTO check_results (execution_id, kind, target, state, detail, latency_ms, checked_at)
		VALUES (?,?,?,?,?,?,?)`,
		c.ExecutionID, c.Kind, c.Target, c.State, c.Detail, c.LatencyMS, formatTime(c.CheckedAt))
	if err != nil {
		return fmt.Errorf("save check %s/%s: %w", c.Kind, c.Target, err)
	}
	if id, err := res.LastInsertId(); err == nil {
		c.ID = id
	}
	return nil
}

// ChecksForExecution returns the checks that ran for one execution.
func (s *Store) ChecksForExecution(ctx context.Context, executionID int64) ([]CheckResult, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT id, execution_id, kind, target, state, detail, latency_ms, checked_at
		FROM check_results WHERE execution_id=? ORDER BY kind, target`, executionID)
	if err != nil {
		return nil, fmt.Errorf("checks for execution %d: %w", executionID, err)
	}
	defer rows.Close()
	return scanChecks(rows)
}

// LatestChecks returns the most recent result for each distinct kind/target,
// which is what the health page renders.
func (s *Store) LatestChecks(ctx context.Context) ([]CheckResult, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT id, execution_id, kind, target, state, detail, latency_ms, checked_at FROM (
			SELECT *, ROW_NUMBER() OVER (PARTITION BY kind, target ORDER BY checked_at DESC, id DESC) AS rn
			FROM check_results
		) WHERE rn = 1
		ORDER BY kind, target`)
	if err != nil {
		return nil, fmt.Errorf("latest checks: %w", err)
	}
	defer rows.Close()
	return scanChecks(rows)
}

func scanChecks(rows *sql.Rows) ([]CheckResult, error) {
	var out []CheckResult
	for rows.Next() {
		var (
			c         CheckResult
			execID    sql.NullInt64
			checkedAt string
		)
		if err := rows.Scan(&c.ID, &execID, &c.Kind, &c.Target, &c.State,
			&c.Detail, &c.LatencyMS, &checkedAt); err != nil {
			return nil, fmt.Errorf("scan check: %w", err)
		}
		if execID.Valid {
			c.ExecutionID = &execID.Int64
		}
		var err error
		if c.CheckedAt, err = parseTime(checkedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RecordNotification logs a dispatched notification for auditing.
func (s *Store) RecordNotification(ctx context.Context, eventType, channel, status, detail string, taskID, executionID *int64) error {
	_, err := s.write.ExecContext(ctx, `
		INSERT INTO notifications (event_type, channel, task_id, execution_id, status, detail, sent_at)
		VALUES (?,?,?,?,?,?,?)`,
		eventType, channel, taskID, executionID, status, detail, formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("record notification: %w", err)
	}
	return nil
}

func nonZeroTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}
