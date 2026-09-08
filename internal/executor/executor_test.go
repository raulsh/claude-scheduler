package executor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/raulsh/claude-scheduler/internal/config"
	"github.com/raulsh/claude-scheduler/internal/store"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	dir := t.TempDir()

	cfg := config.Default()
	cfg.Paths.DataDir = filepath.Join(dir, "data")
	cfg.Paths.LogDir = filepath.Join(dir, "logs")
	// A path that exists, so Locate accepts it; the fake command replaces it.
	cfg.Binaries.Claude = "/bin/sh"
	cfg.Executor.MaxConcurrent = 2
	return cfg
}

func newTestExecutor(t *testing.T, ctx context.Context) (*Executor, *store.Store, config.Config) {
	t.Helper()

	cfg := testConfig(t)
	st, err := store.Open(cfg.Paths.DBPath())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(ctx, cfg, st, NewBroker(), log), st, cfg
}

// fakeCommand replaces the claude CLI with a shell script for the duration of
// a test, so process lifecycle can be verified without spending tokens or
// depending on what the model chooses to do.
func fakeCommand(t *testing.T, script string) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake script: %v", err)
	}

	original := commandContext
	commandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, path)
	}
	t.Cleanup(func() { commandContext = original })
}

func makeTask(t *testing.T, st *store.Store, task *store.Task) *store.Task {
	t.Helper()
	if task.Name == "" {
		task.Name = "test-task"
	}
	if task.CronExpr == "" {
		task.CronExpr = "@daily"
	}
	if task.Prompt == "" {
		task.Prompt = "do the thing"
	}
	task.Enabled = true
	if task.OverlapPolicy == "" {
		task.OverlapPolicy = store.OverlapSkip
	}
	if task.GatingPolicy == "" {
		task.GatingPolicy = store.GateFailFast
	}
	if err := st.CreateTask(context.Background(), task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	return task
}

// TestTimeoutKillsWholeProcessGroup is the regression test for orphaned
// subprocesses. The CLI spawns a shell per Bash tool call, and killing only
// the direct child leaves that shell's own children running.
func TestTimeoutKillsWholeProcessGroup(t *testing.T) {
	childPIDFile := filepath.Join(t.TempDir(), "child.pid")

	// Emit one event, spawn a long-lived grandchild that ignores SIGTERM,
	// record its pid, then block. Only a group-wide SIGKILL can clear this.
	fakeCommand(t, fmt.Sprintf(`
echo '{"type":"system","subtype":"init","session_id":"fake","claude_code_version":"0.0.0-test"}'
sh -c 'trap "" TERM; sleep 120' &
echo $! > %q
wait
`, childPIDFile))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exe, st, _ := newTestExecutor(t, ctx)

	task := makeTask(t, st, &store.Task{Name: "group-kill", TimeoutSeconds: 2})
	task.Timeout = 2 * time.Second

	execution := &store.Execution{TaskID: &task.ID, Trigger: store.TriggerManual}
	if err := st.CreateExecution(context.Background(), execution); err != nil {
		t.Fatalf("create execution: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = exe.Run(ctx, task, execution)
	}()

	// Wait for the grandchild to register itself.
	var childPID int
	for range 50 {
		if data, err := os.ReadFile(childPIDFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				childPID = pid
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("the fake CLI never reported a grandchild pid")
	}
	if !processAlive(childPID) {
		t.Fatalf("grandchild %d was not running", childPID)
	}

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after the timeout")
	}

	// The grandchild traps SIGTERM, so only the escalation to SIGKILL on the
	// process group can have cleared it.
	for range 50 {
		if !processAlive(childPID) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if processAlive(childPID) {
		_ = syscall.Kill(childPID, syscall.SIGKILL) // do not leak from the test
		t.Fatalf("grandchild %d survived the timeout: the process group was not reaped", childPID)
	}

	got, err := st.GetExecution(context.Background(), execution.ID)
	if err != nil {
		t.Fatalf("get execution: %v", err)
	}
	if got.Status != store.StatusTimeout {
		t.Errorf("status = %q, want %q", got.Status, store.StatusTimeout)
	}
}

// processAlive reports whether a pid exists, without affecting it.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	// EPERM means it exists but belongs to another user.
	return err == nil || err == syscall.EPERM
}

func TestCancelStopsRun(t *testing.T) {
	fakeCommand(t, `
echo '{"type":"system","subtype":"init","session_id":"fake"}'
sleep 120
`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exe, st, _ := newTestExecutor(t, ctx)

	task := makeTask(t, st, &store.Task{Name: "cancel-me", TimeoutSeconds: 600})
	task.Timeout = 10 * time.Minute

	execution, err := exe.Trigger(context.Background(), task, store.TriggerManual)
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	// Wait until the run is registered as in flight.
	var cancelled bool
	for range 50 {
		if exe.Cancel(execution.ID) {
			cancelled = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !cancelled {
		t.Fatal("Cancel never found the running execution")
	}

	for range 100 {
		got, err := st.GetExecution(context.Background(), execution.ID)
		if err != nil {
			t.Fatalf("get execution: %v", err)
		}
		if store.Terminal(got.Status) {
			if got.Status != store.StatusCancelled {
				t.Errorf("status = %q, want %q", got.Status, store.StatusCancelled)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("execution never reached a terminal status after cancel")
}

func TestSuccessCapturesResultMetadata(t *testing.T) {
	fakeCommand(t, `
echo '{"type":"system","subtype":"init","session_id":"sess-1","model":"claude-sonnet-5","claude_code_version":"2.1.263","mcp_servers":[{"name":"a","status":"connected"},{"name":"b","status":"needs-auth"}]}'
echo '{"type":"assistant","message":{"model":"claude-sonnet-5","role":"assistant","content":[{"type":"text","text":"done"}]}}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"done","duration_ms":1234,"duration_api_ms":2345,"num_turns":2,"total_cost_usd":0.05,"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40},"permission_denials":[],"terminal_reason":"completed"}'
`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exe, st, cfg := newTestExecutor(t, ctx)

	task := makeTask(t, st, &store.Task{Name: "happy"})
	execution := &store.Execution{TaskID: &task.ID, Trigger: store.TriggerCron}
	if err := st.CreateExecution(context.Background(), execution); err != nil {
		t.Fatalf("create execution: %v", err)
	}
	if err := exe.Run(ctx, task, execution); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := st.GetExecution(context.Background(), execution.ID)
	if err != nil {
		t.Fatalf("get execution: %v", err)
	}

	if got.Status != store.StatusSuccess {
		t.Errorf("status = %q, want success", got.Status)
	}
	if got.ClaudeSessionID != "sess-1" {
		t.Errorf("session = %q", got.ClaudeSessionID)
	}
	if got.ClaudeVersion != "2.1.263" {
		t.Errorf("version = %q", got.ClaudeVersion)
	}
	if got.TotalCostUSD != 0.05 {
		t.Errorf("cost = %v, want 0.05", got.TotalCostUSD)
	}
	if got.NumTurns != 2 {
		t.Errorf("turns = %d, want 2", got.NumTurns)
	}
	if got.CacheReadTokens != 30 || got.CacheCreationTokens != 40 {
		t.Errorf("cache tokens = %d/%d, want 30/40", got.CacheReadTokens, got.CacheCreationTokens)
	}
	if len(got.MCPSnapshot) != 2 {
		t.Errorf("mcp snapshot = %#v", got.MCPSnapshot)
	}

	// The raw stream must also be on disk, where retention can prune it
	// independently of the metadata rows.
	transcript := filepath.Join(cfg.Paths.TranscriptDir(), fmt.Sprintf("%d.ndjson", execution.ID))
	data, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if strings.Count(strings.TrimSpace(string(data)), "\n") != 2 {
		t.Errorf("transcript has %d newlines, want 2 (3 lines)", strings.Count(string(data), "\n"))
	}

	events, err := st.ListEvents(context.Background(), execution.ID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 3 {
		t.Errorf("stored %d events, want 3", len(events))
	}
}

// TestRateLimitIsNotAFailure guards the status the UI renders differently:
// an exhausted credit balance is not a broken task.
func TestRateLimitIsNotAFailure(t *testing.T) {
	fakeCommand(t, `
echo '{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","overageStatus":"rejected","overageDisabledReason":"out_of_credits"}}'
exit 1
`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exe, st, _ := newTestExecutor(t, ctx)

	task := makeTask(t, st, &store.Task{Name: "limited"})
	execution := &store.Execution{TaskID: &task.ID}
	if err := st.CreateExecution(context.Background(), execution); err != nil {
		t.Fatalf("create execution: %v", err)
	}
	if err := exe.Run(ctx, task, execution); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := st.GetExecution(context.Background(), execution.ID)
	if err != nil {
		t.Fatalf("get execution: %v", err)
	}
	if got.Status != store.StatusRateLimited {
		t.Errorf("status = %q, want %q", got.Status, store.StatusRateLimited)
	}
	if !strings.Contains(got.ErrorMessage, "credits") {
		t.Errorf("error message did not explain the limit: %q", got.ErrorMessage)
	}
}

func TestFailureCapturesStderr(t *testing.T) {
	fakeCommand(t, `
echo "something went wrong on stderr" >&2
exit 3
`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exe, st, _ := newTestExecutor(t, ctx)

	task := makeTask(t, st, &store.Task{Name: "broken"})
	execution := &store.Execution{TaskID: &task.ID}
	if err := st.CreateExecution(context.Background(), execution); err != nil {
		t.Fatalf("create execution: %v", err)
	}
	_ = exe.Run(ctx, task, execution)

	got, err := st.GetExecution(context.Background(), execution.ID)
	if err != nil {
		t.Fatalf("get execution: %v", err)
	}
	if got.Status != store.StatusFailure {
		t.Errorf("status = %q, want failure", got.Status)
	}
	if !strings.Contains(got.ErrorMessage, "something went wrong on stderr") {
		t.Errorf("stderr not surfaced: %q", got.ErrorMessage)
	}
	if got.ExitCode == nil || *got.ExitCode != 3 {
		t.Errorf("exit code = %v, want 3", got.ExitCode)
	}
}

// TestOverlapSkipRecordsVisibleRow checks that a suppressed run still shows
// up in history, rather than silently not happening.
func TestOverlapSkipRecordsVisibleRow(t *testing.T) {
	fakeCommand(t, `
echo '{"type":"system","subtype":"init","session_id":"x"}'
sleep 60
`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exe, st, _ := newTestExecutor(t, ctx)

	task := makeTask(t, st, &store.Task{Name: "no-overlap", OverlapPolicy: store.OverlapSkip})
	task.Timeout = time.Minute

	first, err := exe.Trigger(context.Background(), task, store.TriggerCron)
	if err != nil {
		t.Fatalf("first Trigger: %v", err)
	}
	// Let the first run register as in flight.
	for range 50 {
		got, _ := st.GetExecution(context.Background(), first.ID)
		if got != nil && got.Status == store.StatusRunning {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	second, err := exe.Trigger(context.Background(), task, store.TriggerCron)
	if err != nil {
		t.Fatalf("second Trigger: %v", err)
	}
	if second.Status != store.StatusSkipped {
		t.Errorf("second run status = %q, want skipped", second.Status)
	}
	if !strings.Contains(second.ErrorMessage, "still running") {
		t.Errorf("skip reason = %q", second.ErrorMessage)
	}

	exe.Cancel(first.ID)
}
