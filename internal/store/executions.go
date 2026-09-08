package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const execColumns = `e.id, e.task_id, e.trigger, e.status, e.queued_at, e.started_at, e.finished_at,
	e.claude_session_id, e.claude_version, e.model, e.exit_code, e.is_error, e.result_subtype,
	e.terminal_reason, e.num_turns, e.duration_ms, e.duration_api_ms, e.total_cost_usd,
	e.input_tokens, e.output_tokens, e.cache_read_tokens, e.cache_creation_tokens,
	e.permission_denials, e.mcp_snapshot, e.preflight_outcome, e.result_text,
	e.error_message, e.transcript_path`

func scanExecution(sc interface{ Scan(...any) error }) (Execution, error) {
	var (
		e          Execution
		queuedAt   string
		startedAt  sql.NullString
		finishedAt sql.NullString
		denials    string
		snapshot   string
		taskID     sql.NullInt64
		exitCode   sql.NullInt64
		isError    sql.NullBool
	)
	err := sc.Scan(&e.ID, &taskID, &e.Trigger, &e.Status, &queuedAt, &startedAt, &finishedAt,
		&e.ClaudeSessionID, &e.ClaudeVersion, &e.Model, &exitCode, &isError, &e.ResultSubtype,
		&e.TerminalReason, &e.NumTurns, &e.DurationMS, &e.DurationAPIMS, &e.TotalCostUSD,
		&e.InputTokens, &e.OutputTokens, &e.CacheReadTokens, &e.CacheCreationTokens,
		&denials, &snapshot, &e.PreflightOutcome, &e.ResultText, &e.ErrorMessage, &e.TranscriptPath)
	if err != nil {
		return e, err
	}

	if taskID.Valid {
		e.TaskID = &taskID.Int64
	}
	if exitCode.Valid {
		code := int(exitCode.Int64)
		e.ExitCode = &code
	}
	if isError.Valid {
		e.IsError = &isError.Bool
	}
	if e.QueuedAt, err = parseTime(queuedAt); err != nil {
		return e, err
	}
	if e.StartedAt, err = nullTime(startedAt); err != nil {
		return e, err
	}
	if e.FinishedAt, err = nullTime(finishedAt); err != nil {
		return e, err
	}

	// Malformed JSON in these columns should degrade to empty, not break the
	// whole execution record.
	_ = json.Unmarshal([]byte(denials), &e.PermissionDenials)
	_ = json.Unmarshal([]byte(snapshot), &e.MCPSnapshot)
	if e.PermissionDenials == nil {
		e.PermissionDenials = []string{}
	}
	if e.MCPSnapshot == nil {
		e.MCPSnapshot = []MCPRef{}
	}
	return e, nil
}

// CreateExecution inserts a new execution row and populates its ID.
func (s *Store) CreateExecution(ctx context.Context, e *Execution) error {
	if e.Status == "" {
		e.Status = StatusPending
	}
	if e.Trigger == "" {
		e.Trigger = TriggerCron
	}
	if e.PreflightOutcome == "" {
		e.PreflightOutcome = "ok"
	}
	if e.QueuedAt.IsZero() {
		e.QueuedAt = time.Now()
	}

	res, err := s.write.ExecContext(ctx, `
		INSERT INTO executions (task_id, trigger, status, queued_at, model, preflight_outcome, transcript_path)
		VALUES (?,?,?,?,?,?,?)`,
		e.TaskID, e.Trigger, e.Status, formatTime(e.QueuedAt), e.Model,
		e.PreflightOutcome, e.TranscriptPath)
	if err != nil {
		return fmt.Errorf("insert execution: %w", err)
	}
	if e.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("execution id: %w", err)
	}
	return nil
}

// MarkExecutionStarted records the transition into running.
func (s *Store) MarkExecutionStarted(ctx context.Context, id int64, at time.Time) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE executions SET status=?, started_at=? WHERE id=?`,
		StatusRunning, formatTime(at), id)
	if err != nil {
		return fmt.Errorf("mark execution %d started: %w", id, err)
	}
	return nil
}

// FinishExecution writes the terminal state and all result metadata.
func (s *Store) FinishExecution(ctx context.Context, e *Execution) error {
	denials, err := json.Marshal(orEmpty(e.PermissionDenials))
	if err != nil {
		return fmt.Errorf("marshal permission_denials: %w", err)
	}
	snapshot, err := json.Marshal(orEmpty(e.MCPSnapshot))
	if err != nil {
		return fmt.Errorf("marshal mcp_snapshot: %w", err)
	}
	if e.FinishedAt.IsZero() {
		e.FinishedAt = time.Now()
	}

	_, err = s.write.ExecContext(ctx, `
		UPDATE executions SET status=?, finished_at=?, claude_session_id=?, claude_version=?,
			model=?, exit_code=?, is_error=?, result_subtype=?, terminal_reason=?, num_turns=?,
			duration_ms=?, duration_api_ms=?, total_cost_usd=?, input_tokens=?, output_tokens=?,
			cache_read_tokens=?, cache_creation_tokens=?, permission_denials=?, mcp_snapshot=?,
			preflight_outcome=?, result_text=?, error_message=?, transcript_path=?
		WHERE id=?`,
		e.Status, timeArg(e.FinishedAt), e.ClaudeSessionID, e.ClaudeVersion, e.Model,
		e.ExitCode, e.IsError, e.ResultSubtype, e.TerminalReason, e.NumTurns,
		e.DurationMS, e.DurationAPIMS, e.TotalCostUSD, e.InputTokens, e.OutputTokens,
		e.CacheReadTokens, e.CacheCreationTokens, string(denials), string(snapshot),
		e.PreflightOutcome, e.ResultText, e.ErrorMessage, e.TranscriptPath, e.ID)
	if err != nil {
		return fmt.Errorf("finish execution %d: %w", e.ID, err)
	}
	return nil
}

// GetExecution loads one execution with its check results.
func (s *Store) GetExecution(ctx context.Context, id int64) (*Execution, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+execColumns+`, COALESCE(t.name,'') FROM executions e
		 LEFT JOIN tasks t ON t.id = e.task_id WHERE e.id=?`, id)

	e, err := scanExecutionWithName(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get execution %d: %w", id, err)
	}

	if e.Checks, err = s.ChecksForExecution(ctx, id); err != nil {
		return nil, err
	}
	return &e, nil
}

func scanExecutionWithName(sc interface{ Scan(...any) error }) (Execution, error) {
	// The task name is appended to the standard column list, so wrap the
	// scanner to consume it after the execution fields.
	var name string
	wrapped := scannerFunc(func(dest ...any) error {
		return sc.Scan(append(dest, &name)...)
	})
	e, err := scanExecution(wrapped)
	if err != nil {
		return e, err
	}
	e.TaskName = name
	return e, nil
}

type scannerFunc func(dest ...any) error

func (f scannerFunc) Scan(dest ...any) error { return f(dest...) }

// ExecutionFilter narrows an execution listing. Every field is optional.
type ExecutionFilter struct {
	TaskID   *int64
	Statuses []string
	From     time.Time
	To       time.Time
	Query    string
	Limit    int
	Cursor   string
}

// ListExecutions returns a page of executions newest-first plus the cursor for
// the following page, empty when the listing is exhausted.
func (s *Store) ListExecutions(ctx context.Context, f ExecutionFilter) ([]Execution, string, error) {
	where := []string{"1=1"}
	var args []any

	if f.TaskID != nil {
		where = append(where, "e.task_id = ?")
		args = append(args, *f.TaskID)
	}
	if len(f.Statuses) > 0 {
		where = append(where, "e.status IN ("+placeholders(len(f.Statuses))+")")
		for _, st := range f.Statuses {
			args = append(args, st)
		}
	}
	if !f.From.IsZero() {
		where = append(where, "e.queued_at >= ?")
		args = append(args, formatTime(f.From))
	}
	if !f.To.IsZero() {
		where = append(where, "e.queued_at <= ?")
		args = append(args, formatTime(f.To))
	}
	if f.Query != "" {
		where = append(where, "(e.result_text LIKE ? OR e.error_message LIKE ? OR COALESCE(t.name,'') LIKE ?)")
		like := "%" + f.Query + "%"
		args = append(args, like, like, like)
	}
	if f.Cursor != "" {
		queuedAt, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", err
		}
		// A row-value comparison keeps pagination correct when several
		// executions share a timestamp, which happens whenever a fan-out
		// queues them in the same millisecond.
		where = append(where, "(e.queued_at, e.id) < (?, ?)")
		args = append(args, queuedAt, id)
	}

	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	// Fetch one extra row to detect whether a further page exists.
	args = append(args, limit+1)

	rows, err := s.read.QueryContext(ctx,
		`SELECT `+execColumns+`, COALESCE(t.name,'') FROM executions e
		 LEFT JOIN tasks t ON t.id = e.task_id
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY e.queued_at DESC, e.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list executions: %w", err)
	}
	defer rows.Close()

	var out []Execution
	for rows.Next() {
		e, err := scanExecutionWithName(rows)
		if err != nil {
			return nil, "", fmt.Errorf("scan execution: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = encodeCursor(last.QueuedAt, last.ID)
	}
	return out, next, nil
}

// LastExecutionsByTask returns the most recent n executions for every task,
// newest-first. One query serves the whole task list, which is what keeps the
// dashboard's status strips off the N+1 path.
func (s *Store) LastExecutionsByTask(ctx context.Context, n int) (map[int64][]Execution, error) {
	if n <= 0 {
		n = 5
	}
	rows, err := s.read.QueryContext(ctx, `
		SELECT `+execColumns+`, '' FROM (
			SELECT *, ROW_NUMBER() OVER (
				PARTITION BY task_id ORDER BY queued_at DESC, id DESC
			) AS rn
			FROM executions WHERE task_id IS NOT NULL
		) e
		WHERE e.rn <= ?
		ORDER BY e.task_id, e.queued_at DESC, e.id DESC`, n)
	if err != nil {
		return nil, fmt.Errorf("last executions: %w", err)
	}
	defer rows.Close()

	byTask := make(map[int64][]Execution)
	for rows.Next() {
		e, err := scanExecutionWithName(rows)
		if err != nil {
			return nil, fmt.Errorf("scan last execution: %w", err)
		}
		if e.TaskID != nil {
			byTask[*e.TaskID] = append(byTask[*e.TaskID], e)
		}
	}
	return byTask, rows.Err()
}

// RunningExecutionsForTask counts executions still in flight, which the
// overlap policy consults before firing a new one.
func (s *Store) RunningExecutionsForTask(ctx context.Context, taskID int64) (int, error) {
	var n int
	err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM executions WHERE task_id=? AND status IN (?,?)`,
		taskID, StatusPending, StatusRunning).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count running for task %d: %w", taskID, err)
	}
	return n, nil
}

// ReapOrphanedExecutions marks executions that were in flight when the
// service stopped. Without this they would appear to run forever.
func (s *Store) ReapOrphanedExecutions(ctx context.Context) (int64, error) {
	res, err := s.write.ExecContext(ctx, `
		UPDATE executions SET status=?, finished_at=?, error_message=?
		WHERE status IN (?,?)`,
		StatusCancelled, formatTime(time.Now()),
		"interrupted: the scheduler stopped while this execution was in flight",
		StatusPending, StatusRunning)
	if err != nil {
		return 0, fmt.Errorf("reap orphaned executions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// encodeCursor renders a pagination position. Both the timestamp and the id
// are needed: ordering is by timestamp, and the id breaks ties.
func encodeCursor(queuedAt time.Time, id int64) string {
	raw := formatTime(queuedAt) + "|" + strconv.FormatInt(id, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(cursor string) (queuedAt string, id int64, err error) {
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", 0, fmt.Errorf("invalid cursor %q", cursor)
	}
	ts, rawID, found := strings.Cut(string(decoded), "|")
	if !found {
		return "", 0, fmt.Errorf("invalid cursor %q", cursor)
	}
	id, err = strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("invalid cursor %q", cursor)
	}
	return ts, id, nil
}
