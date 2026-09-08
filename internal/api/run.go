package api

import (
	"context"
	"net/http"

	"github.com/raulsh/claude-scheduler/internal/store"
)

// handleRunTask queues an immediate run of a task, bypassing its schedule
// but not its pre-flight checks.
func (s *Server) handleRunTask(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid task id")
		return
	}
	if s.runner == nil {
		writeError(w, http.StatusServiceUnavailable, "the executor is not available")
		return
	}

	task, err := s.store.GetTask(r.Context(), id)
	if err != nil {
		writeStoreError(w, err, "get task")
		return
	}

	// A manual run deliberately ignores the paused flag: pausing stops the
	// schedule, and the user asking for a run right now is an override.
	exec, err := s.runner.Trigger(r.Context(), task, store.TriggerManual)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "trigger run: "+err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, exec)
}

// handleCancelExecution stops a running execution.
func (s *Server) handleCancelExecution(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid execution id")
		return
	}
	if s.executor == nil {
		writeError(w, http.StatusServiceUnavailable, "the executor is not available")
		return
	}

	if !s.executor.Cancel(id) {
		// Either it already finished or this process never ran it.
		exec, err := s.store.GetExecution(r.Context(), id)
		if err != nil {
			writeStoreError(w, err, "get execution")
			return
		}
		if store.Terminal(exec.Status) {
			writeError(w, http.StatusConflict, "execution already finished with status "+exec.Status)
			return
		}
		writeError(w, http.StatusConflict, "execution is not running in this process")
		return
	}

	s.log.Info("execution cancelled via API", "execution", id)
	writeJSON(w, http.StatusAccepted, map[string]any{"cancelled": id})
}

// Runner is the executor-facing behaviour the API needs. Keeping it an
// interface lets the API be tested without spawning real processes.
type Runner interface {
	Trigger(ctx context.Context, task *store.Task, trigger string) (*store.Execution, error)
}

// handleRerunWithAllowlist is the approval loop.
//
// A scheduled run denies anything outside its allowlist rather than hanging
// on a prompt nobody can answer, and reports what it denied. This endpoint
// appends those rules to the task's allowlist and starts a fresh run, so
// approving is one click instead of a trip through the edit form. The
// allowlist teaches itself over time.
func (s *Server) handleRerunWithAllowlist(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid execution id")
		return
	}
	if s.runner == nil {
		writeError(w, http.StatusServiceUnavailable, "the executor is not available")
		return
	}

	exec, err := s.store.GetExecution(r.Context(), id)
	if err != nil {
		writeStoreError(w, err, "get execution")
		return
	}
	if exec.TaskID == nil {
		writeError(w, http.StatusUnprocessableEntity,
			"this execution is not attached to a task, so there is no allowlist to extend")
		return
	}
	if len(exec.PermissionDenials) == 0 {
		writeError(w, http.StatusUnprocessableEntity,
			"this execution recorded no permission denials")
		return
	}

	task, err := s.store.GetTask(r.Context(), *exec.TaskID)
	if err != nil {
		writeStoreError(w, err, "get task")
		return
	}

	added := mergeRules(task.AllowedTools, exec.PermissionDenials)
	if len(added) > 0 {
		task.AllowedTools = append(task.AllowedTools, added...)
		if err := s.store.UpdateTask(r.Context(), task); err != nil {
			writeStoreError(w, err, "extend the allowlist")
			return
		}
		s.log.Info("allowlist extended from denials",
			"task", task.Name, "execution", id, "added", added)
	}

	// A manual trigger, so it still honours pre-flight checks but not the
	// paused flag: the user is asking for this run explicitly.
	next, err := s.runner.Trigger(r.Context(), task, store.TriggerRetry)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "start the re-run: "+err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"execution":     next,
		"rules_added":   orEmpty(added),
		"allowed_tools": task.AllowedTools,
	})
}

// mergeRules returns the denied rules not already covered by the allowlist.
func mergeRules(existing, denied []string) []string {
	have := make(map[string]bool, len(existing))
	for _, rule := range existing {
		have[rule] = true
	}

	var added []string
	for _, rule := range denied {
		if rule == "" || have[rule] {
			continue
		}
		have[rule] = true
		added = append(added, rule)
	}
	return added
}
