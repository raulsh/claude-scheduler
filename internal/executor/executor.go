// Package executor runs scheduled Claude Code invocations, streams their
// output to subscribers and persists the transcript.
package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/raulsh/claude-scheduler/internal/claude"
	"github.com/raulsh/claude-scheduler/internal/config"
	"github.com/raulsh/claude-scheduler/internal/store"
)

// stderrLimit bounds how much of the CLI's stderr is retained for the error
// message on a failed run.
const stderrLimit = 16 << 10

// termGrace is how long a cancelled run has to exit after SIGTERM before the
// process group is killed outright.
const termGrace = 5 * time.Second

// Executor owns the lifecycle of running executions.
type Executor struct {
	cfg    config.Config
	store  *store.Store
	broker *Broker
	log    *slog.Logger

	// sem bounds how many runs execute at once.
	sem chan struct{}

	// baseCtx is the service lifetime. Background runs use it so they are
	// not cancelled when the HTTP request that started them returns.
	baseCtx context.Context

	mu      sync.Mutex
	running map[int64]*handle
	// pendingChecks holds passing pre-flight results between the gate and
	// the execution row they get attached to.
	pendingChecks map[int64][]store.CheckResult

	preflight Preflight
	// notify is called with each terminal outcome. A function rather than an
	// interface keeps the notify package out of the executor's imports.
	notify func(task *store.Task, exec *store.Execution)
	// notifyPause is called when a gating failure pauses a schedule.
	notifyPause func(task *store.Task, reason string)
}

// SetNotifier installs the terminal-outcome callback.
func (e *Executor) SetNotifier(fn func(task *store.Task, exec *store.Execution)) {
	e.notify = fn
}

// SetPauseNotifier installs the schedule-auto-paused callback.
func (e *Executor) SetPauseNotifier(fn func(task *store.Task, reason string)) {
	e.notifyPause = fn
}

type handle struct {
	cancel context.CancelFunc
	// reason records why a run was stopped, so the outcome can distinguish a
	// user cancellation from a timeout.
	reason string
}

// New creates an executor. The context bounds every background run, so
// cancelling it stops all in-flight executions at shutdown.
func New(ctx context.Context, cfg config.Config, st *store.Store, broker *Broker, log *slog.Logger) *Executor {
	maxConcurrent := cfg.Executor.MaxConcurrent
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &Executor{
		cfg:     cfg,
		store:   st,
		broker:  broker,
		log:     log,
		baseCtx: ctx,
		sem:     make(chan struct{}, maxConcurrent),
		running: make(map[int64]*handle),
	}
}

// Broker exposes the event broker for the API's SSE handler.
func (e *Executor) Broker() *Broker { return e.broker }

// Running reports the currently executing execution IDs.
func (e *Executor) Running() []int64 {
	e.mu.Lock()
	defer e.mu.Unlock()

	ids := make([]int64, 0, len(e.running))
	for id := range e.running {
		ids = append(ids, id)
	}
	return ids
}

// Cancel stops a running execution, killing its whole process group.
func (e *Executor) Cancel(executionID int64) bool {
	e.mu.Lock()
	h, ok := e.running[executionID]
	if ok {
		h.reason = store.StatusCancelled
	}
	e.mu.Unlock()

	if !ok {
		return false
	}
	h.cancel()
	return true
}

// Run executes a task synchronously and records the outcome. The execution
// row must already exist; the caller owns pre-flight gating.
func (e *Executor) Run(ctx context.Context, task *store.Task, exec *store.Execution) error {
	// Wait for a concurrency slot, but abandon the run if the service is
	// shutting down while queued.
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return e.abandon(exec, store.StatusCancelled, "cancelled while queued for a concurrency slot")
	}

	cli, err := claude.Locate(e.cfg.Binaries.Claude)
	if err != nil {
		return e.abandon(exec, store.StatusFailure, err.Error())
	}

	timeout := task.Timeout
	if timeout <= 0 {
		timeout = e.cfg.Executor.DefaultTimeout.Std()
	}
	budget := task.MaxBudgetUSD
	if budget <= 0 {
		budget = e.cfg.Executor.DefaultMaxBudget
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	h := &handle{cancel: cancel}
	e.mu.Lock()
	e.running[exec.ID] = h
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.running, exec.ID)
		e.mu.Unlock()
	}()

	// The timeout is tracked separately from cancellation so the outcome can
	// tell "it ran too long" from "someone stopped it".
	timer := time.AfterFunc(timeout, func() {
		e.mu.Lock()
		if h.reason == "" {
			h.reason = store.StatusTimeout
		}
		e.mu.Unlock()
		cancel()
	})
	defer timer.Stop()

	sessionID := uuid.NewString()
	opts := claude.RunOptions{
		Prompt:            task.Prompt,
		Model:             task.Model,
		SessionID:         sessionID,
		Cwd:               task.Cwd,
		AllowedTools:      task.AllowedTools,
		Tools:             task.Tools,
		BypassPermissions: task.BypassPermissions,
		MaxBudgetUSD:      budget,
	}

	args := cli.Args(opts)
	cmd := commandContext(runCtx, cli.Path, args...)
	cmd.Dir = workingDir(task.Cwd)
	cmd.Env = os.Environ()
	// A dedicated process group means cancelling kills the CLI's children
	// too; without it they outlive the run and keep working.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = strings.NewReader(task.Prompt)

	// CommandContext's default cancel signals only the direct child, which
	// leaves the CLI's own subprocesses running. Because Setpgid gives the
	// child a new group whose id equals its pid, the whole group can be
	// signalled instead. This closure runs after Start, so cmd.Process is set.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	// If the group ignores SIGTERM, force the child down and close its pipes
	// so the stream reader is not left blocked on a grandchild holding stdout.
	cmd.WaitDelay = termGrace

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return e.abandon(exec, store.StatusFailure, "stdout pipe: "+err.Error())
	}
	stderr := &boundedBuffer{limit: stderrLimit}
	cmd.Stderr = stderr

	transcript, transcriptPath, err := e.openTranscript(exec.ID)
	if err != nil {
		e.log.Warn("could not open transcript file; continuing without it",
			"execution", exec.ID, "error", err)
	}
	if transcript != nil {
		defer transcript.Close()
		exec.TranscriptPath = transcriptPath
	}

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return e.abandon(exec, store.StatusFailure, "start claude: "+err.Error())
	}
	// Recorded while the process is certainly alive: after Wait the leader is
	// reaped and its group id can no longer be looked up.
	pgid := cmd.Process.Pid

	exec.Status = store.StatusRunning
	exec.StartedAt = started
	if err := e.store.MarkExecutionStarted(runCtx, exec.ID, started); err != nil {
		e.log.Warn("could not mark execution started", "execution", exec.ID, "error", err)
	}
	e.broker.Publish(exec.ID, Message{Type: "status", Status: store.StatusRunning})

	e.log.Info("execution started",
		"execution", exec.ID, "task", task.Name, "binary", cli.Resolved(),
		"session", sessionID, "timeout", timeout, "budget_usd", budget)

	// The raw stream is tee'd to disk so a very large transcript lives on the
	// filesystem, where retention can prune it independently of the metadata.
	var source io.Reader = stdout
	if transcript != nil {
		source = io.TeeReader(stdout, transcript)
	}

	summary := e.consume(runCtx, exec.ID, source)

	waitErr := cmd.Wait()
	// Sweep up anything the CLI left behind in its group.
	reapProcessGroup(pgid, e.log)

	e.mu.Lock()
	stopReason := h.reason
	e.mu.Unlock()

	e.finish(ctx, task, exec, summary, waitErr, stderr.String(), stopReason, started)
	return nil
}

// consume reads the NDJSON stream, persisting and publishing each event.
func (e *Executor) consume(ctx context.Context, executionID int64, r io.Reader) claude.Summary {
	dec := claude.NewDecoder(r)
	var summary claude.Summary

	for {
		ev, skipped, err := dec.Next()
		for _, line := range skipped {
			e.log.Debug("skipped non-JSON output line",
				"execution", executionID, "line", truncate(line, 200))
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				e.log.Warn("stream decode ended early", "execution", executionID, "error", err)
			}
			return summary
		}

		summary.Observe(ev)
		seq := dec.Seq()

		// Persistence uses a background context: when a run is cancelled the
		// events already received are still worth keeping.
		storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = e.store.AppendEvent(storeCtx, &store.ExecutionEvent{
			ExecutionID: executionID,
			Seq:         seq,
			TS:          ev.Timestamp,
			Type:        ev.Type,
			Subtype:     ev.Subtype,
			Payload:     ev.Raw,
		})
		cancel()
		if err != nil {
			e.log.Warn("could not persist event", "execution", executionID, "seq", seq, "error", err)
		}

		e.broker.Publish(executionID, Message{
			Seq:     seq,
			Type:    ev.Type,
			Subtype: ev.Subtype,
			Payload: ev.Raw,
		})
	}
}

// finish classifies the outcome and writes the terminal record.
func (e *Executor) finish(
	ctx context.Context,
	task *store.Task,
	exec *store.Execution,
	summary claude.Summary,
	waitErr error,
	stderrText string,
	stopReason string,
	started time.Time,
) {
	// Persist regardless of why the run ended.
	ctx = context.WithoutCancel(ctx)

	exec.FinishedAt = time.Now()
	exec.ClaudeSessionID = summary.SessionID
	exec.ClaudeVersion = summary.Version
	if summary.Model != "" {
		exec.Model = summary.Model
	}

	for _, m := range summary.MCPServers {
		exec.MCPSnapshot = append(exec.MCPSnapshot, store.MCPRef{Name: m.Name, Status: m.Status})
	}

	if code, ok := exitCode(waitErr); ok {
		exec.ExitCode = &code
	}

	if summary.SawResult {
		r := summary.Result
		isErr := r.IsError
		exec.IsError = &isErr
		exec.ResultSubtype = r.Subtype
		exec.TerminalReason = r.TerminalReason
		exec.NumTurns = r.NumTurns
		exec.DurationMS = r.DurationMS
		exec.DurationAPIMS = r.DurationAPIMS
		exec.TotalCostUSD = r.TotalCostUSD
		exec.InputTokens = r.Usage.InputTokens
		exec.OutputTokens = r.Usage.OutputTokens
		exec.CacheReadTokens = r.Usage.CacheReadTokens
		exec.CacheCreationTokens = r.Usage.CacheCreationTokens
		exec.ResultText = r.Result
		exec.PermissionDenials = r.DenialRules()
	}
	if exec.DurationMS == 0 {
		exec.DurationMS = time.Since(started).Milliseconds()
	}

	exec.Status, exec.ErrorMessage = classify(summary, waitErr, stopReason, stderrText)

	if err := e.store.FinishExecution(ctx, exec); err != nil {
		e.log.Error("could not record execution outcome", "execution", exec.ID, "error", err)
	}

	e.broker.Publish(exec.ID, Message{
		Type:     "status",
		Status:   exec.Status,
		Terminal: true,
	})
	e.broker.Close(exec.ID)

	e.log.Info("execution finished",
		"execution", exec.ID, "task", task.Name, "status", exec.Status,
		"duration_ms", exec.DurationMS, "cost_usd", exec.TotalCostUSD,
		"turns", exec.NumTurns, "denials", len(exec.PermissionDenials))

	if e.notify != nil {
		e.notify(task, exec)
	}
}

// classify maps the raw outcome onto an execution status.
//
// The ordering matters: an explicit stop reason wins over anything the
// process reported, and a credit exhaustion is reported separately from a
// task defect so it does not look like broken automation.
func classify(summary claude.Summary, waitErr error, stopReason, stderrText string) (status, message string) {
	switch stopReason {
	case store.StatusTimeout:
		return store.StatusTimeout, "the execution exceeded its timeout and was stopped"
	case store.StatusCancelled:
		return store.StatusCancelled, "cancelled"
	}

	if summary.SawResult {
		if summary.Result.IsError {
			msg := summary.Result.Result
			if msg == "" {
				msg = "claude reported an error"
			}
			// The CLI reports a hit budget ceiling as its own result subtype.
			// It is a configured limit rather than a broken task, so it reads
			// as a rate limit rather than a failure.
			if summary.Result.Subtype == "error_max_budget_usd" {
				return store.StatusRateLimited,
					"the run stopped because it reached its budget cap"
			}
			if summary.RateLimited {
				return store.StatusRateLimited, summary.RateLimitDetail
			}
			return store.StatusFailure, msg
		}
		// A clean result stands even if a rate-limit notice arrived, since
		// the work completed.
		return store.StatusSuccess, ""
	}

	// No result event: the run did not complete a turn.
	if summary.RateLimited {
		return store.StatusRateLimited, summary.RateLimitDetail
	}
	if waitErr != nil {
		msg := "claude exited without producing a result: " + waitErr.Error()
		if trimmed := strings.TrimSpace(stderrText); trimmed != "" {
			msg += "\n" + truncate(trimmed, 2000)
		}
		return store.StatusFailure, msg
	}
	msg := "claude exited without producing a result event"
	if trimmed := strings.TrimSpace(stderrText); trimmed != "" {
		msg += "\n" + truncate(trimmed, 2000)
	}
	return store.StatusFailure, msg
}

// abandon records a run that never got as far as starting the CLI.
func (e *Executor) abandon(exec *store.Execution, status, message string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	exec.Status = status
	exec.ErrorMessage = message
	exec.FinishedAt = time.Now()
	if err := e.store.FinishExecution(ctx, exec); err != nil {
		e.log.Error("could not record abandoned execution", "execution", exec.ID, "error", err)
	}

	e.broker.Publish(exec.ID, Message{Type: "status", Status: status, Terminal: true})
	e.broker.Close(exec.ID)
	e.log.Warn("execution abandoned", "execution", exec.ID, "status", status, "reason", message)
	return errors.New(message)
}

// openTranscript creates the raw NDJSON sink for an execution.
func (e *Executor) openTranscript(executionID int64) (*os.File, string, error) {
	dir := e.cfg.Paths.TranscriptDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, "", fmt.Errorf("create transcript dir: %w", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%d.ndjson", executionID))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return nil, "", fmt.Errorf("open transcript: %w", err)
	}
	return f, path, nil
}

// reapProcessGroup terminates anything still alive in a finished run's
// process group. The CLI spawns subprocesses (a bash shell per Bash tool
// call, and whatever that shell runs), and a normal wait reaps only the
// leader, so without this sweep a cancelled run can leave work behind.
//
// pgid must have been captured while the leader was alive.
func reapProcessGroup(pgid int, log *slog.Logger) {
	if pgid <= 1 {
		return
	}

	// Signal 0 probes for surviving members without affecting them.
	if err := syscall.Kill(-pgid, 0); err != nil {
		return // Already empty: the common case.
	}

	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		log.Debug("SIGTERM to process group failed", "pgid", pgid, "error", err)
	}

	deadline := time.Now().Add(termGrace)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	log.Warn("process group survived SIGTERM; sending SIGKILL", "pgid", pgid)
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		log.Warn("SIGKILL to process group failed", "pgid", pgid, "error", err)
	}
}

func exitCode(err error) (int, bool) {
	if err == nil {
		return 0, true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), true
	}
	return 0, false
}

// workingDir falls back to a directory that certainly exists, since the CLI
// refuses to start in a missing one.
func workingDir(cwd string) string {
	if cwd == "" {
		return os.TempDir()
	}
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return os.TempDir()
	}
	return cwd
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "... (truncated)"
}

// commandContext is indirected so tests can substitute a fake command.
var commandContext = exec.CommandContext
