package executor

import (
	"context"
	"fmt"

	"github.com/raulsh/claude-scheduler/internal/store"
)

// Trigger creates an execution for a task and runs it in the background,
// honouring the task's overlap policy. It returns as soon as the execution
// row exists, so callers do not block on the run.
func (e *Executor) Trigger(ctx context.Context, task *store.Task, trigger string) (*store.Execution, error) {
	// Dependencies are checked before anything else: the point of the gate
	// is to avoid spending tokens against a broken environment.
	if blocked, err := e.gate(ctx, task, trigger); err != nil {
		return nil, err
	} else if blocked != nil {
		return blocked, nil
	}

	if task.OverlapPolicy == store.OverlapSkip {
		inFlight, err := e.store.RunningExecutionsForTask(ctx, task.ID)
		if err != nil {
			return nil, fmt.Errorf("check for in-flight runs: %w", err)
		}
		if inFlight > 0 {
			return e.recordSkipped(ctx, task, trigger,
				fmt.Sprintf("skipped: %d execution(s) of this task were still running", inFlight))
		}
	}

	exec := &store.Execution{
		TaskID:  &task.ID,
		Trigger: trigger,
		Model:   task.Model,
	}
	if err := e.store.CreateExecution(ctx, exec); err != nil {
		return nil, fmt.Errorf("create execution: %w", err)
	}

	// Attach the pre-flight results that let this run proceed, so the
	// execution detail shows what was verified rather than only what failed.
	for _, check := range e.takeChecks(task.ID) {
		check.ExecutionID = &exec.ID
		if err := e.store.SaveCheckResult(ctx, &check); err != nil {
			e.log.Warn("could not attach pre-flight result to execution",
				"execution", exec.ID, "check", check.Kind+"/"+check.Target, "error", err)
		}
	}

	// The run outlives the request that started it, so it gets the service
	// lifetime rather than the caller's context.
	go func() {
		if err := e.Run(e.baseCtx, task, exec); err != nil {
			e.log.Warn("execution ended with an error",
				"execution", exec.ID, "task", task.Name, "error", err)
		}
	}()

	return exec, nil
}

// recordSkipped writes a terminal skipped execution, so a suppressed run is
// visible in the history rather than silently absent.
func (e *Executor) recordSkipped(ctx context.Context, task *store.Task, trigger, reason string) (*store.Execution, error) {
	exec := &store.Execution{
		TaskID:  &task.ID,
		Trigger: trigger,
		Model:   task.Model,
	}
	if err := e.store.CreateExecution(ctx, exec); err != nil {
		return nil, fmt.Errorf("create skipped execution: %w", err)
	}

	exec.Status = store.StatusSkipped
	exec.ErrorMessage = reason
	exec.PreflightOutcome = "skipped"
	if err := e.store.FinishExecution(ctx, exec); err != nil {
		return nil, fmt.Errorf("record skipped execution: %w", err)
	}

	e.log.Info("execution skipped", "task", task.Name, "reason", reason)
	return exec, nil
}

// RecordBlocked writes a terminal blocked execution together with the check
// results that caused it. A gated run stays visible in the history instead
// of looking like a run that never happened.
func (e *Executor) RecordBlocked(
	ctx context.Context,
	task *store.Task,
	trigger string,
	failing []store.CheckResult,
	reason string,
) (*store.Execution, error) {
	exec := &store.Execution{
		TaskID:  &task.ID,
		Trigger: trigger,
		Model:   task.Model,
	}
	if err := e.store.CreateExecution(ctx, exec); err != nil {
		return nil, fmt.Errorf("create blocked execution: %w", err)
	}

	for i := range failing {
		failing[i].ExecutionID = &exec.ID
		if err := e.store.SaveCheckResult(ctx, &failing[i]); err != nil {
			e.log.Warn("could not record gating check result",
				"execution", exec.ID, "check", failing[i].Kind+"/"+failing[i].Target, "error", err)
		}
	}

	exec.Status = store.StatusBlocked
	exec.ErrorMessage = reason
	exec.PreflightOutcome = "blocked"
	if err := e.store.FinishExecution(ctx, exec); err != nil {
		return nil, fmt.Errorf("record blocked execution: %w", err)
	}

	e.log.Warn("execution blocked by pre-flight checks",
		"task", task.Name, "execution", exec.ID, "reason", reason)

	if e.notify != nil {
		e.notify(task, exec)
	}
	return exec, nil
}
