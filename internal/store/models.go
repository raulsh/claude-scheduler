package store

import "time"

// Requirement kinds. These map one-to-one onto healthcheck Checker kinds.
const (
	KindAWSProfile = "aws_profile"
	KindMCPServer  = "mcp_server"
	KindBinary     = "binary"
)

// Execution statuses. The user-facing strip collapses these to colours, but
// the distinctions matter: Blocked never ran, RateLimited is not a defect.
const (
	StatusPending     = "pending"
	StatusRunning     = "running"
	StatusSuccess     = "success"
	StatusFailure     = "failure"
	StatusBlocked     = "blocked"
	StatusRateLimited = "rate_limited"
	StatusTimeout     = "timeout"
	StatusCancelled   = "cancelled"
	StatusSkipped     = "skipped"
)

// Terminal reports whether a status means the execution has finished.
func Terminal(status string) bool {
	switch status {
	case StatusPending, StatusRunning:
		return false
	default:
		return true
	}
}

// Trigger values.
const (
	TriggerCron   = "cron"
	TriggerManual = "manual"
	TriggerRetry  = "retry"
)

// Overlap policies decide what happens when a task is still running at its
// next fire time.
const (
	OverlapSkip     = "skip"
	OverlapQueue    = "queue"
	OverlapParallel = "parallel"
)

// Gating policies decide what happens when pre-flight checks fail.
const (
	GateFailFast      = "fail_fast"
	GateSkip          = "skip"
	GatePauseSchedule = "pause_schedule"
)

// Check states. Distinguishing NeedsLogin from Unavailable is what makes
// auto-pause safe: a flaky network must never pause a schedule.
const (
	CheckOK            = "ok"
	CheckNeedsLogin    = "needs_login"
	CheckMisconfigured = "misconfigured"
	CheckUnavailable   = "unavailable"
	CheckUnknown       = "unknown"
)

// Task is a scheduled Claude Code invocation.
type Task struct {
	ID             int64         `json:"id"`
	Name           string        `json:"name"`
	Description    string        `json:"description"`
	CronExpr       string        `json:"cron_expr"`
	Timezone       string        `json:"timezone"`
	Prompt         string        `json:"prompt"`
	Model          string        `json:"model"`
	Cwd            string        `json:"cwd"`
	Timeout        time.Duration `json:"-"`
	TimeoutSeconds int64         `json:"timeout_seconds"`
	MaxBudgetUSD   float64       `json:"max_budget_usd"`
	AllowedTools   []string      `json:"allowed_tools"`
	// Tools restricts which built-in tools exist for the run. Unlike
	// AllowedTools, which pre-approves actions that would otherwise prompt,
	// this removes tools outright. Empty means the CLI's default set.
	Tools             []string  `json:"tools"`
	BypassPermissions bool      `json:"bypass_permissions"`
	OverlapPolicy     string    `json:"overlap_policy"`
	GatingPolicy      string    `json:"gating_policy"`
	Enabled           bool      `json:"enabled"`
	Paused            bool      `json:"paused"`
	PausedReason      string    `json:"paused_reason"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`

	// Populated by list/detail queries, not stored on the row.
	// These are never omitempty: a task with no runs must still serialise
	// last_executions as [], or every client needs a missing-key check
	// before it can render.
	Requirements   []Requirement `json:"requirements"`
	LastExecutions []Execution   `json:"last_executions"`
	NextRunAt      *time.Time    `json:"next_run_at,omitempty"`
}

// Requirement is one declared dependency of a task.
type Requirement struct {
	ID       int64  `json:"id"`
	TaskID   int64  `json:"task_id"`
	Kind     string `json:"kind"`
	Target   string `json:"target"`
	Required bool   `json:"required"`
}

// Execution is one run, attempted or completed.
type Execution struct {
	ID                  int64     `json:"id"`
	TaskID              *int64    `json:"task_id"`
	Trigger             string    `json:"trigger"`
	Status              string    `json:"status"`
	QueuedAt            time.Time `json:"queued_at"`
	StartedAt           time.Time `json:"started_at,omitzero"`
	FinishedAt          time.Time `json:"finished_at,omitzero"`
	ClaudeSessionID     string    `json:"claude_session_id"`
	ClaudeVersion       string    `json:"claude_version"`
	Model               string    `json:"model"`
	ExitCode            *int      `json:"exit_code"`
	IsError             *bool     `json:"is_error"`
	ResultSubtype       string    `json:"result_subtype"`
	TerminalReason      string    `json:"terminal_reason"`
	NumTurns            int       `json:"num_turns"`
	DurationMS          int64     `json:"duration_ms"`
	DurationAPIMS       int64     `json:"duration_api_ms"`
	TotalCostUSD        float64   `json:"total_cost_usd"`
	InputTokens         int64     `json:"input_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	CacheReadTokens     int64     `json:"cache_read_tokens"`
	CacheCreationTokens int64     `json:"cache_creation_tokens"`
	PermissionDenials   []string  `json:"permission_denials"`
	MCPSnapshot         []MCPRef  `json:"mcp_snapshot"`
	PreflightOutcome    string    `json:"preflight_outcome"`
	ResultText          string    `json:"result_text"`
	ErrorMessage        string    `json:"error_message"`
	TranscriptPath      string    `json:"transcript_path"`

	// Populated by detail queries.
	Checks   []CheckResult `json:"checks,omitempty"`
	TaskName string        `json:"task_name,omitempty"`
}

// MCPRef is one entry of the mcp_servers array in a system/init event.
type MCPRef struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// ExecutionEvent is one parsed line of the NDJSON stream.
type ExecutionEvent struct {
	ID          int64     `json:"id"`
	ExecutionID int64     `json:"execution_id"`
	Seq         int64     `json:"seq"`
	TS          time.Time `json:"ts"`
	Type        string    `json:"type"`
	Subtype     string    `json:"subtype"`
	Payload     []byte    `json:"payload"`
}

// CheckResult is one healthcheck outcome.
type CheckResult struct {
	ID          int64     `json:"id"`
	ExecutionID *int64    `json:"execution_id,omitempty"`
	Kind        string    `json:"kind"`
	Target      string    `json:"target"`
	State       string    `json:"state"`
	Detail      string    `json:"detail"`
	LatencyMS   int64     `json:"latency_ms"`
	CheckedAt   time.Time `json:"checked_at"`
}

// Gates reports whether a check state should stop an execution.
// Unavailable deliberately does not gate: transient network failure must not
// masquerade as a broken credential.
func Gates(state string) bool {
	switch state {
	case CheckNeedsLogin, CheckMisconfigured:
		return true
	default:
		return false
	}
}
