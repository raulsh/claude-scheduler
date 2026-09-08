package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

const taskColumns = `id, name, description, cron_expr, timezone, prompt, model, cwd,
	timeout_seconds, max_budget_usd, allowed_tools, tools, bypass_permissions,
	overlap_policy, gating_policy, enabled, paused, paused_reason, created_at, updated_at`

func scanTask(sc interface{ Scan(...any) error }) (Task, error) {
	var (
		t          Task
		allowRules string
		toolSet    string
		createdAt  string
		updatedAt  string
	)
	err := sc.Scan(&t.ID, &t.Name, &t.Description, &t.CronExpr, &t.Timezone, &t.Prompt,
		&t.Model, &t.Cwd, &t.TimeoutSeconds, &t.MaxBudgetUSD, &allowRules, &toolSet,
		&t.BypassPermissions, &t.OverlapPolicy, &t.GatingPolicy, &t.Enabled, &t.Paused,
		&t.PausedReason, &createdAt, &updatedAt)
	if err != nil {
		return t, err
	}

	t.Timeout = time.Duration(t.TimeoutSeconds) * time.Second
	// A corrupt JSON column must not make the whole task unreadable; the
	// empty value is the safe interpretation in both cases.
	if err := json.Unmarshal([]byte(allowRules), &t.AllowedTools); err != nil {
		t.AllowedTools = nil
	}
	if err := json.Unmarshal([]byte(toolSet), &t.Tools); err != nil {
		t.Tools = nil
	}
	if t.CreatedAt, err = parseTime(createdAt); err != nil {
		return t, err
	}
	if t.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return t, err
	}
	return t, nil
}

// CreateTask inserts a task and populates its ID.
func (s *Store) CreateTask(ctx context.Context, t *Task) error {
	allowRules, err := json.Marshal(orEmpty(t.AllowedTools))
	if err != nil {
		return fmt.Errorf("marshal allowed_tools: %w", err)
	}
	toolSet, err := json.Marshal(orEmpty(t.Tools))
	if err != nil {
		return fmt.Errorf("marshal tools: %w", err)
	}

	now := time.Now()
	nowStr := formatTime(now)
	t.CreatedAt, t.UpdatedAt = now, now

	res, err := s.write.ExecContext(ctx, `
		INSERT INTO tasks (name, description, cron_expr, timezone, prompt, model, cwd,
			timeout_seconds, max_budget_usd, allowed_tools, tools, bypass_permissions,
			overlap_policy, gating_policy, enabled, paused, paused_reason, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.Name, t.Description, t.CronExpr, t.Timezone, t.Prompt, t.Model, t.Cwd,
		t.TimeoutSeconds, t.MaxBudgetUSD, string(allowRules), string(toolSet),
		t.BypassPermissions, t.OverlapPolicy, t.GatingPolicy, t.Enabled, t.Paused,
		t.PausedReason, nowStr, nowStr)
	if err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	if t.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("task id: %w", err)
	}

	if len(t.Requirements) > 0 {
		if err := s.ReplaceRequirements(ctx, t.ID, t.Requirements); err != nil {
			return err
		}
	}
	return nil
}

// UpdateTask writes every mutable field of an existing task.
func (s *Store) UpdateTask(ctx context.Context, t *Task) error {
	allowRules, err := json.Marshal(orEmpty(t.AllowedTools))
	if err != nil {
		return fmt.Errorf("marshal allowed_tools: %w", err)
	}
	toolSet, err := json.Marshal(orEmpty(t.Tools))
	if err != nil {
		return fmt.Errorf("marshal tools: %w", err)
	}

	t.UpdatedAt = time.Now()
	res, err := s.write.ExecContext(ctx, `
		UPDATE tasks SET name=?, description=?, cron_expr=?, timezone=?, prompt=?, model=?,
			cwd=?, timeout_seconds=?, max_budget_usd=?, allowed_tools=?, tools=?,
			bypass_permissions=?, overlap_policy=?, gating_policy=?, enabled=?, paused=?,
			paused_reason=?, updated_at=?
		WHERE id=?`,
		t.Name, t.Description, t.CronExpr, t.Timezone, t.Prompt, t.Model, t.Cwd,
		t.TimeoutSeconds, t.MaxBudgetUSD, string(allowRules), string(toolSet),
		t.BypassPermissions, t.OverlapPolicy, t.GatingPolicy, t.Enabled, t.Paused,
		t.PausedReason, formatTime(t.UpdatedAt), t.ID)
	if err != nil {
		return fmt.Errorf("update task: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return s.ReplaceRequirements(ctx, t.ID, t.Requirements)
}

// GetTask loads one task with its requirements.
func (s *Store) GetTask(ctx context.Context, id int64) (*Task, error) {
	row := s.read.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=?`, id)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get task %d: %w", id, err)
	}

	if t.Requirements, err = s.RequirementsFor(ctx, id); err != nil {
		return nil, err
	}
	return &t, nil
}

// ListTasks returns all tasks with their requirements attached.
func (s *Store) ListTasks(ctx context.Context) ([]Task, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+taskColumns+` FROM tasks ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	var tasks []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	reqs, err := s.allRequirements(ctx)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		tasks[i].Requirements = reqs[tasks[i].ID]
	}
	return tasks, nil
}

// DeleteTask removes a task; its requirements and executions cascade.
func (s *Store) DeleteTask(ctx context.Context, id int64) error {
	res, err := s.write.ExecContext(ctx, `DELETE FROM tasks WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete task %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetTaskPaused pauses or resumes a task, recording why.
func (s *Store) SetTaskPaused(ctx context.Context, id int64, paused bool, reason string) error {
	if !paused {
		reason = ""
	}
	_, err := s.write.ExecContext(ctx,
		`UPDATE tasks SET paused=?, paused_reason=?, updated_at=? WHERE id=?`,
		paused, reason, formatTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("set paused on task %d: %w", id, err)
	}
	return nil
}

// ReplaceRequirements swaps a task's declared dependencies for the given set.
func (s *Store) ReplaceRequirements(ctx context.Context, taskID int64, reqs []Requirement) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin requirements tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM task_requirements WHERE task_id=?`, taskID); err != nil {
		return fmt.Errorf("clear requirements: %w", err)
	}
	for _, r := range reqs {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO task_requirements (task_id, kind, target, required) VALUES (?,?,?,?)`,
			taskID, r.Kind, r.Target, r.Required); err != nil {
			return fmt.Errorf("insert requirement %s/%s: %w", r.Kind, r.Target, err)
		}
	}
	return tx.Commit()
}

// RequirementsFor returns one task's declared dependencies.
func (s *Store) RequirementsFor(ctx context.Context, taskID int64) ([]Requirement, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, task_id, kind, target, required FROM task_requirements
		 WHERE task_id=? ORDER BY kind, target`, taskID)
	if err != nil {
		return nil, fmt.Errorf("requirements for task %d: %w", taskID, err)
	}
	defer rows.Close()
	return scanRequirements(rows)
}

func (s *Store) allRequirements(ctx context.Context) (map[int64][]Requirement, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, task_id, kind, target, required FROM task_requirements ORDER BY task_id, kind, target`)
	if err != nil {
		return nil, fmt.Errorf("all requirements: %w", err)
	}
	defer rows.Close()

	reqs, err := scanRequirements(rows)
	if err != nil {
		return nil, err
	}
	byTask := make(map[int64][]Requirement, len(reqs))
	for _, r := range reqs {
		byTask[r.TaskID] = append(byTask[r.TaskID], r)
	}
	return byTask, nil
}

func scanRequirements(rows *sql.Rows) ([]Requirement, error) {
	var out []Requirement
	for rows.Next() {
		var r Requirement
		if err := rows.Scan(&r.ID, &r.TaskID, &r.Kind, &r.Target, &r.Required); err != nil {
			return nil, fmt.Errorf("scan requirement: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// orEmpty keeps JSON columns as [] rather than null.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// DependencyUsage counts how many enabled tasks declare each dependency.
//
// This is what separates a dependency that actually matters from one that is
// merely present: a machine can have dozens of configured MCP connectors
// that no schedule references, and counting those as problems would make the
// warning banner meaningless.
func (s *Store) DependencyUsage(ctx context.Context) (map[string]int, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT r.kind, r.target, COUNT(*) FROM task_requirements r
		JOIN tasks t ON t.id = r.task_id
		WHERE r.required = 1 AND t.enabled = 1
		GROUP BY r.kind, r.target`)
	if err != nil {
		return nil, fmt.Errorf("dependency usage: %w", err)
	}
	defer rows.Close()

	usage := make(map[string]int)
	for rows.Next() {
		var (
			kind   string
			target string
			count  int
		)
		if err := rows.Scan(&kind, &target, &count); err != nil {
			return nil, fmt.Errorf("scan dependency usage: %w", err)
		}
		usage[DependencyKey(kind, target)] = count
	}
	return usage, rows.Err()
}

// DependencyKey identifies a dependency across kinds.
func DependencyKey(kind, target string) string { return kind + "\x00" + target }
