package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	for i := range 2 {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		if err := s.Ping(context.Background()); err != nil {
			t.Fatalf("Ping #%d: %v", i, err)
		}
		s.Close()
	}
}

func TestTaskRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	task := &Task{
		Name:           "aws-canary",
		CronExpr:       "*/15 * * * *",
		Timezone:       "America/Santiago",
		Prompt:         "Run aws sts get-caller-identity and report the account.",
		Model:          "sonnet",
		TimeoutSeconds: 600,
		MaxBudgetUSD:   0.5,
		AllowedTools:   []string{"Bash(aws *)", "Read"},
		OverlapPolicy:  OverlapSkip,
		GatingPolicy:   GateFailFast,
		Enabled:        true,
		Requirements: []Requirement{
			{Kind: KindAWSProfile, Target: "admin", Required: true},
			{Kind: KindBinary, Target: "aws", Required: true},
		},
	}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.ID == 0 {
		t.Fatal("CreateTask left ID unset")
	}

	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Name != task.Name || got.CronExpr != task.CronExpr {
		t.Errorf("round trip mismatch: got %+v", got)
	}
	if len(got.AllowedTools) != 2 || got.AllowedTools[0] != "Bash(aws *)" {
		t.Errorf("allowed_tools not preserved: %#v", got.AllowedTools)
	}
	if got.Timeout != 600*time.Second {
		t.Errorf("Timeout = %v, want 10m", got.Timeout)
	}
	if len(got.Requirements) != 2 {
		t.Fatalf("got %d requirements, want 2", len(got.Requirements))
	}

	// Requirements are replaced wholesale, not merged.
	got.Requirements = []Requirement{{Kind: KindMCPServer, Target: "lemn-signals", Required: true}}
	got.Description = "updated"
	if err := s.UpdateTask(ctx, got); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	reloaded, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask after update: %v", err)
	}
	if len(reloaded.Requirements) != 1 || reloaded.Requirements[0].Kind != KindMCPServer {
		t.Errorf("requirements not replaced: %#v", reloaded.Requirements)
	}
	if reloaded.Description != "updated" {
		t.Errorf("Description = %q", reloaded.Description)
	}
}

func TestGetTaskNotFound(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.GetTask(context.Background(), 4242); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestExecutionLifecycleAndFilters(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	task := &Task{Name: "t", CronExpr: "@hourly", Prompt: "p", Enabled: true,
		OverlapPolicy: OverlapSkip, GatingPolicy: GateFailFast}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	exec := &Execution{TaskID: &task.ID, Trigger: TriggerManual}
	if err := s.CreateExecution(ctx, exec); err != nil {
		t.Fatalf("CreateExecution: %v", err)
	}
	if exec.Status != StatusPending {
		t.Errorf("Status = %q, want pending", exec.Status)
	}

	if n, _ := s.RunningExecutionsForTask(ctx, task.ID); n != 1 {
		t.Errorf("running count = %d, want 1", n)
	}

	if err := s.MarkExecutionStarted(ctx, exec.ID, time.Now()); err != nil {
		t.Fatalf("MarkExecutionStarted: %v", err)
	}

	exec.Status = StatusSuccess
	exec.TotalCostUSD = 0.0604938
	exec.NumTurns = 1
	exec.ClaudeVersion = "2.1.263"
	exec.ResultText = "ok"
	exec.MCPSnapshot = []MCPRef{{Name: "lemn-signals", Status: "connected"}}
	exec.PermissionDenials = []string{"Bash(rm *)"}
	if err := s.FinishExecution(ctx, exec); err != nil {
		t.Fatalf("FinishExecution: %v", err)
	}

	got, err := s.GetExecution(ctx, exec.ID)
	if err != nil {
		t.Fatalf("GetExecution: %v", err)
	}
	if got.Status != StatusSuccess || got.TotalCostUSD == 0 {
		t.Errorf("terminal state not persisted: %+v", got)
	}
	if got.TaskName != "t" {
		t.Errorf("TaskName = %q, want t", got.TaskName)
	}
	if len(got.MCPSnapshot) != 1 || got.MCPSnapshot[0].Status != "connected" {
		t.Errorf("MCPSnapshot = %#v", got.MCPSnapshot)
	}
	if len(got.PermissionDenials) != 1 {
		t.Errorf("PermissionDenials = %#v", got.PermissionDenials)
	}
	if got.FinishedAt.IsZero() {
		t.Error("FinishedAt is zero")
	}

	// Status filter must exclude non-matching rows.
	list, _, err := s.ListExecutions(ctx, ExecutionFilter{Statuses: []string{StatusFailure}})
	if err != nil {
		t.Fatalf("ListExecutions: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("failure filter returned %d rows, want 0", len(list))
	}

	list, _, err = s.ListExecutions(ctx, ExecutionFilter{TaskID: &task.ID, Statuses: []string{StatusSuccess}})
	if err != nil {
		t.Fatalf("ListExecutions: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("success filter returned %d rows, want 1", len(list))
	}
}

func TestLastExecutionsByTaskWindow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	task := &Task{Name: "windowed", CronExpr: "@hourly", Prompt: "p", Enabled: true,
		OverlapPolicy: OverlapSkip, GatingPolicy: GateFailFast}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Seven runs; only the newest five should come back.
	for i := range 7 {
		e := &Execution{TaskID: &task.ID}
		if err := s.CreateExecution(ctx, e); err != nil {
			t.Fatalf("CreateExecution %d: %v", i, err)
		}
		e.Status = StatusSuccess
		if err := s.FinishExecution(ctx, e); err != nil {
			t.Fatalf("FinishExecution %d: %v", i, err)
		}
	}

	byTask, err := s.LastExecutionsByTask(ctx, 5)
	if err != nil {
		t.Fatalf("LastExecutionsByTask: %v", err)
	}
	got := byTask[task.ID]
	if len(got) != 5 {
		t.Fatalf("got %d executions, want 5", len(got))
	}
	// Newest first.
	for i := 1; i < len(got); i++ {
		if got[i-1].ID < got[i].ID {
			t.Errorf("not ordered newest-first: %d before %d", got[i-1].ID, got[i].ID)
		}
	}
}

func TestPaginationCursor(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	task := &Task{Name: "paged", CronExpr: "@hourly", Prompt: "p", Enabled: true,
		OverlapPolicy: OverlapSkip, GatingPolicy: GateFailFast}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	for range 5 {
		if err := s.CreateExecution(ctx, &Execution{TaskID: &task.ID}); err != nil {
			t.Fatalf("CreateExecution: %v", err)
		}
	}

	page1, next, err := s.ListExecutions(ctx, ExecutionFilter{Limit: 2})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page1) != 2 || next == "" {
		t.Fatalf("page 1 = %d rows, next=%q", len(page1), next)
	}

	page2, _, err := s.ListExecutions(ctx, ExecutionFilter{Limit: 2, Cursor: next})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page 2 = %d rows, want 2", len(page2))
	}
	if page1[0].ID == page2[0].ID {
		t.Error("cursor did not advance")
	}
}

func TestEventsAppendAndResume(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	e := &Execution{}
	if err := s.CreateExecution(ctx, e); err != nil {
		t.Fatalf("CreateExecution: %v", err)
	}

	for i := int64(1); i <= 3; i++ {
		ev := &ExecutionEvent{ExecutionID: e.ID, Seq: i, Type: "assistant", Payload: []byte(`{"a":1}`)}
		if err := s.AppendEvent(ctx, ev); err != nil {
			t.Fatalf("AppendEvent %d: %v", i, err)
		}
	}
	// A replayed sequence number must not duplicate or error.
	dup := &ExecutionEvent{ExecutionID: e.ID, Seq: 2, Type: "assistant", Payload: []byte(`{"a":1}`)}
	if err := s.AppendEvent(ctx, dup); err != nil {
		t.Fatalf("AppendEvent duplicate: %v", err)
	}

	all, err := s.ListEvents(ctx, e.ID, 0, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d events, want 3", len(all))
	}

	// Resume semantics: everything after seq 1.
	rest, err := s.ListEvents(ctx, e.ID, 1, 0)
	if err != nil {
		t.Fatalf("ListEvents resume: %v", err)
	}
	if len(rest) != 2 || rest[0].Seq != 2 {
		t.Errorf("resume returned %d events starting at %d", len(rest), rest[0].Seq)
	}

	max, err := s.MaxEventSeq(ctx, e.ID)
	if err != nil {
		t.Fatalf("MaxEventSeq: %v", err)
	}
	if max != 3 {
		t.Errorf("MaxEventSeq = %d, want 3", max)
	}
}

func TestLatestChecksDeduplicates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	older := &CheckResult{Kind: KindAWSProfile, Target: "admin", State: CheckNeedsLogin,
		CheckedAt: time.Now().Add(-time.Hour)}
	newer := &CheckResult{Kind: KindAWSProfile, Target: "admin", State: CheckOK,
		CheckedAt: time.Now()}
	for _, c := range []*CheckResult{older, newer} {
		if err := s.SaveCheckResult(ctx, c); err != nil {
			t.Fatalf("SaveCheckResult: %v", err)
		}
	}

	latest, err := s.LatestChecks(ctx)
	if err != nil {
		t.Fatalf("LatestChecks: %v", err)
	}
	if len(latest) != 1 {
		t.Fatalf("got %d checks, want 1 deduplicated", len(latest))
	}
	if latest[0].State != CheckOK {
		t.Errorf("State = %q, want the newer ok", latest[0].State)
	}
}

func TestReapOrphanedExecutions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateExecution(ctx, &Execution{}); err != nil {
		t.Fatalf("CreateExecution: %v", err)
	}
	n, err := s.ReapOrphanedExecutions(ctx)
	if err != nil {
		t.Fatalf("ReapOrphanedExecutions: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped %d, want 1", n)
	}

	list, _, err := s.ListExecutions(ctx, ExecutionFilter{})
	if err != nil {
		t.Fatalf("ListExecutions: %v", err)
	}
	if list[0].Status != StatusCancelled {
		t.Errorf("Status = %q, want cancelled", list[0].Status)
	}
}

func TestGatesIgnoresTransientFailure(t *testing.T) {
	// A flaky network must never gate an execution; a dead credential must.
	cases := map[string]bool{
		CheckOK:            false,
		CheckUnavailable:   false,
		CheckUnknown:       false,
		CheckNeedsLogin:    true,
		CheckMisconfigured: true,
	}
	for state, want := range cases {
		if got := Gates(state); got != want {
			t.Errorf("Gates(%q) = %v, want %v", state, got, want)
		}
	}
}

// TestExecutionOrderingFollowsTimestamp guards against ordering by insertion
// id while the UI displays queued_at. The two disagree whenever rows are
// written out of chronological order, as happens with a backfill, an import,
// or a catch-up run recorded after a later manual one.
func TestExecutionOrderingFollowsTimestamp(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	task := &Task{Name: "ordered", CronExpr: "@hourly", Prompt: "p", Enabled: true,
		OverlapPolicy: OverlapSkip, GatingPolicy: GateFailFast}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Truncated to the millisecond that storage keeps, so comparisons below
	// are exact rather than fighting sub-millisecond rounding.
	base := time.Now().Add(-24 * time.Hour).Truncate(time.Millisecond)
	// Inserted oldest-last, so insertion order is the reverse of time order.
	offsets := []time.Duration{0, 6 * time.Hour, 3 * time.Hour, 12 * time.Hour, 1 * time.Hour}
	for _, offset := range offsets {
		e := &Execution{TaskID: &task.ID, QueuedAt: base.Add(offset)}
		if err := s.CreateExecution(ctx, e); err != nil {
			t.Fatalf("CreateExecution: %v", err)
		}
		e.Status = StatusSuccess
		e.FinishedAt = e.QueuedAt.Add(time.Second)
		if err := s.FinishExecution(ctx, e); err != nil {
			t.Fatalf("FinishExecution: %v", err)
		}
	}

	list, _, err := s.ListExecutions(ctx, ExecutionFilter{})
	if err != nil {
		t.Fatalf("ListExecutions: %v", err)
	}
	if len(list) != len(offsets) {
		t.Fatalf("got %d executions, want %d", len(list), len(offsets))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].QueuedAt.Before(list[i].QueuedAt) {
			t.Errorf("not newest-first at index %d: %s before %s",
				i, list[i-1].QueuedAt.Format(time.RFC3339), list[i].QueuedAt.Format(time.RFC3339))
		}
	}

	// The per-task window must agree with the list ordering.
	byTask, err := s.LastExecutionsByTask(ctx, 3)
	if err != nil {
		t.Fatalf("LastExecutionsByTask: %v", err)
	}
	window := byTask[task.ID]
	if len(window) != 3 {
		t.Fatalf("window has %d rows, want 3", len(window))
	}
	if !window[0].QueuedAt.Equal(base.Add(12 * time.Hour)) {
		t.Errorf("window[0] queued at %s, want the newest (%s)",
			window[0].QueuedAt.Format(time.RFC3339), base.Add(12*time.Hour).Format(time.RFC3339))
	}
	for i := 1; i < len(window); i++ {
		if window[i-1].QueuedAt.Before(window[i].QueuedAt) {
			t.Errorf("window not newest-first at %d", i)
		}
	}
}

// TestCursorPaginationWithTiedTimestamps covers the case a plain timestamp
// cursor would break on: several executions queued in the same instant.
func TestCursorPaginationWithTiedTimestamps(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	task := &Task{Name: "tied", CronExpr: "@hourly", Prompt: "p", Enabled: true,
		OverlapPolicy: OverlapParallel, GatingPolicy: GateFailFast}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Six rows sharing one timestamp.
	same := time.Now().Truncate(time.Second)
	for range 6 {
		if err := s.CreateExecution(ctx, &Execution{TaskID: &task.ID, QueuedAt: same}); err != nil {
			t.Fatalf("CreateExecution: %v", err)
		}
	}

	seen := map[int64]bool{}
	cursor := ""
	for page := range 5 {
		list, next, err := s.ListExecutions(ctx, ExecutionFilter{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, e := range list {
			if seen[e.ID] {
				t.Fatalf("execution %d returned on more than one page", e.ID)
			}
			seen[e.ID] = true
		}
		if next == "" {
			break
		}
		cursor = next
	}

	if len(seen) != 6 {
		t.Errorf("paged through %d distinct executions, want 6", len(seen))
	}
}

func TestInvalidCursorIsRejected(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.ListExecutions(context.Background(), ExecutionFilter{Cursor: "!!not-base64!!"}); err == nil {
		t.Error("a malformed cursor should be rejected")
	}
}
