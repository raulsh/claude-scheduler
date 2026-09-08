package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/raulsh/claude-scheduler/internal/store"
)

// Preflight evaluates a task's declared dependencies before it runs.
//
// It returns every result for the record, plus the subset that should stop
// the execution. It is a function rather than an interface so the executor
// does not depend on the health package, which keeps both testable in
// isolation.
type Preflight func(ctx context.Context, task *store.Task) (all, failing []store.CheckResult)

// SetPreflight installs the pre-flight evaluator. A nil evaluator disables
// gating, which is what the executor's own tests use.
func (e *Executor) SetPreflight(p Preflight) { e.preflight = p }

// gate runs pre-flight checks and decides whether the task may proceed.
// The returned execution is non-nil when the run was suppressed.
func (e *Executor) gate(ctx context.Context, task *store.Task, trigger string) (*store.Execution, error) {
	if e.preflight == nil || len(task.Requirements) == 0 {
		return nil, nil
	}

	all, failing := e.preflight(ctx, task)
	if len(failing) == 0 {
		// Nothing to do; the results are recorded against the execution once
		// it exists, by the caller.
		e.stashChecks(task.ID, all)
		return nil, nil
	}

	reason := describeFailures(failing)

	switch task.GatingPolicy {
	case store.GateSkip:
		exec, err := e.recordSkipped(ctx, task, trigger, "pre-flight checks failed: "+reason)
		return exec, err

	case store.GatePauseSchedule:
		exec, err := e.RecordBlocked(ctx, task, trigger, failing, reason)
		if err != nil {
			return nil, err
		}
		if e.notifyPause != nil {
			e.notifyPause(task, reason)
		}
		// Only a dependency needing human attention pauses a schedule.
		// A transient failure never reaches here, because it does not gate.
		if err := e.store.SetTaskPaused(ctx, task.ID, true, reason); err != nil {
			e.log.Warn("could not pause task after a failed pre-flight",
				"task", task.Name, "error", err)
		} else {
			e.log.Warn("task paused until its dependencies are fixed",
				"task", task.Name, "reason", reason)
		}
		return exec, nil

	default: // store.GateFailFast
		exec, err := e.RecordBlocked(ctx, task, trigger, failing, reason)
		return exec, err
	}
}

// stashChecks holds passing results so they can be attached to the execution
// row once it is created.
func (e *Executor) stashChecks(taskID int64, results []store.CheckResult) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pendingChecks == nil {
		e.pendingChecks = make(map[int64][]store.CheckResult)
	}
	e.pendingChecks[taskID] = results
}

// takeChecks retrieves and clears the stashed results for a task.
func (e *Executor) takeChecks(taskID int64) []store.CheckResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	results := e.pendingChecks[taskID]
	delete(e.pendingChecks, taskID)
	return results
}

// describeFailures renders a human-readable gating reason.
func describeFailures(failing []store.CheckResult) string {
	parts := make([]string, 0, len(failing))
	for _, f := range failing {
		part := fmt.Sprintf("%s %q is %s", readableKind(f.Kind), f.Target, readableState(f.State))
		if f.Detail != "" {
			part += " (" + f.Detail + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

func readableKind(kind string) string {
	switch kind {
	case store.KindAWSProfile:
		return "AWS profile"
	case store.KindMCPServer:
		return "MCP server"
	case store.KindBinary:
		return "binary"
	default:
		return kind
	}
}

func readableState(state string) string {
	switch state {
	case store.CheckNeedsLogin:
		return "not logged in"
	case store.CheckMisconfigured:
		return "misconfigured"
	case store.CheckUnavailable:
		return "unavailable"
	default:
		return state
	}
}
