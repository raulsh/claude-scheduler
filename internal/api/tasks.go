package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/raulsh/claude-scheduler/internal/schedule"
	"github.com/raulsh/claude-scheduler/internal/store"
)

// taskRequest is the wire shape for creating and updating a task. It is kept
// separate from store.Task so clients cannot set server-owned fields.
type taskRequest struct {
	Name              string             `json:"name"`
	Description       string             `json:"description"`
	CronExpr          string             `json:"cron_expr"`
	Timezone          string             `json:"timezone"`
	Prompt            string             `json:"prompt"`
	Model             string             `json:"model"`
	Cwd               string             `json:"cwd"`
	TimeoutSeconds    int64              `json:"timeout_seconds"`
	MaxBudgetUSD      float64            `json:"max_budget_usd"`
	AllowedTools      []string           `json:"allowed_tools"`
	Tools             []string           `json:"tools"`
	BypassPermissions bool               `json:"bypass_permissions"`
	OverlapPolicy     string             `json:"overlap_policy"`
	GatingPolicy      string             `json:"gating_policy"`
	Enabled           *bool              `json:"enabled"`
	Paused            *bool              `json:"paused"`
	Requirements      []requirementInput `json:"requirements"`
}

type requirementInput struct {
	Kind     string `json:"kind"`
	Target   string `json:"target"`
	Required *bool  `json:"required"`
}

// validate normalizes defaults and reports every problem at once, so the
// form can show all field errors rather than one per round trip.
func (tr *taskRequest) validate() []string {
	var problems []string

	tr.Name = strings.TrimSpace(tr.Name)
	if tr.Name == "" {
		problems = append(problems, "name is required")
	}
	if strings.TrimSpace(tr.Prompt) == "" {
		problems = append(problems, "prompt is required")
	}

	if tr.Timezone == "" {
		tr.Timezone = "Local"
	}
	if strings.TrimSpace(tr.CronExpr) == "" {
		problems = append(problems, "cron_expr is required")
	} else if _, err := schedule.Parse(tr.CronExpr, tr.Timezone); err != nil {
		problems = append(problems, err.Error())
	}

	if tr.OverlapPolicy == "" {
		tr.OverlapPolicy = store.OverlapSkip
	}
	switch tr.OverlapPolicy {
	case store.OverlapSkip, store.OverlapQueue, store.OverlapParallel:
	default:
		problems = append(problems, "overlap_policy must be skip, queue or parallel")
	}

	if tr.GatingPolicy == "" {
		tr.GatingPolicy = store.GateFailFast
	}
	switch tr.GatingPolicy {
	case store.GateFailFast, store.GateSkip, store.GatePauseSchedule:
	default:
		problems = append(problems, "gating_policy must be fail_fast, skip or pause_schedule")
	}

	if tr.TimeoutSeconds < 0 {
		problems = append(problems, "timeout_seconds cannot be negative")
	}
	if tr.MaxBudgetUSD < 0 {
		problems = append(problems, "max_budget_usd cannot be negative")
	}

	for i, req := range tr.Requirements {
		switch req.Kind {
		case store.KindAWSProfile, store.KindMCPServer, store.KindBinary:
		default:
			problems = append(problems, "requirements["+strconv.Itoa(i)+"].kind must be aws_profile, mcp_server or binary")
		}
		if strings.TrimSpace(req.Target) == "" {
			problems = append(problems, "requirements["+strconv.Itoa(i)+"].target is required")
		}
	}
	return problems
}

// applyTo copies the request onto a task, leaving server-owned fields alone.
func (tr *taskRequest) applyTo(t *store.Task) {
	t.Name = tr.Name
	t.Description = tr.Description
	t.CronExpr = tr.CronExpr
	t.Timezone = tr.Timezone
	t.Prompt = tr.Prompt
	t.Model = tr.Model
	t.Cwd = tr.Cwd
	t.TimeoutSeconds = tr.TimeoutSeconds
	t.MaxBudgetUSD = tr.MaxBudgetUSD
	t.AllowedTools = tr.AllowedTools
	t.Tools = tr.Tools
	t.BypassPermissions = tr.BypassPermissions
	t.OverlapPolicy = tr.OverlapPolicy
	t.GatingPolicy = tr.GatingPolicy

	if tr.Enabled != nil {
		t.Enabled = *tr.Enabled
	}
	if tr.Paused != nil {
		t.Paused = *tr.Paused
		if !t.Paused {
			t.PausedReason = ""
		}
	}

	t.Requirements = make([]store.Requirement, 0, len(tr.Requirements))
	for _, r := range tr.Requirements {
		required := true
		if r.Required != nil {
			required = *r.Required
		}
		t.Requirements = append(t.Requirements, store.Requirement{
			Kind:     r.Kind,
			Target:   strings.TrimSpace(r.Target),
			Required: required,
		})
	}
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	tasks, err := s.store.ListTasks(ctx)
	if err != nil {
		writeStoreError(w, err, "list tasks")
		return
	}

	// One query for every task's recent runs keeps the dashboard off the
	// N+1 path, which matters because each row renders a five-run strip.
	recent, err := s.store.LastExecutionsByTask(ctx, 5)
	if err != nil {
		writeStoreError(w, err, "load recent executions")
		return
	}

	for i := range tasks {
		// Always an array, never null: every client would otherwise need a
		// nil check before rendering the status strip.
		tasks[i].LastExecutions = orEmpty(recent[tasks[i].ID])
		tasks[i].Requirements = orEmpty(tasks[i].Requirements)
		if next, err := nextRun(tasks[i]); err == nil && !next.IsZero() {
			tasks[i].NextRunAt = &next
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"tasks": orEmpty(tasks)})
}

// nextRun computes the upcoming fire time server-side, so the UI and the
// scheduler can never disagree about timezone handling.
func nextRun(t store.Task) (time.Time, error) {
	if !t.Enabled || t.Paused {
		return time.Time{}, nil
	}
	sched, err := schedule.Parse(t.CronExpr, t.Timezone)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(time.Now()), nil
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req taskRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if problems := req.validate(); len(problems) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":    "validation failed",
			"problems": problems,
		})
		return
	}

	task := &store.Task{Enabled: true}
	req.applyTo(task)

	if err := s.store.CreateTask(r.Context(), task); err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "a task named "+task.Name+" already exists")
			return
		}
		writeStoreError(w, err, "create task")
		return
	}

	s.reloadSchedules(r.Context())
	s.log.Info("task created", "id", task.ID, "name", task.Name)
	writeJSON(w, http.StatusCreated, s.reload(r, task))
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid task id")
		return
	}

	task, err := s.store.GetTask(r.Context(), id)
	if err != nil {
		writeStoreError(w, err, "get task")
		return
	}
	if next, err := nextRun(*task); err == nil && !next.IsZero() {
		task.NextRunAt = &next
	}
	task.Requirements = orEmpty(task.Requirements)
	// The detail page renders the same five-run strip as the list, so the
	// recent executions have to be attached here too.
	if recent, err := s.store.LastExecutionsByTask(r.Context(), 5); err == nil {
		task.LastExecutions = orEmpty(recent[task.ID])
	} else {
		s.log.Warn("could not load recent executions for task", "task", task.ID, "error", err)
		task.LastExecutions = []store.Execution{}
	}
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) handleUpdateTask(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid task id")
		return
	}

	existing, err := s.store.GetTask(r.Context(), id)
	if err != nil {
		writeStoreError(w, err, "get task")
		return
	}

	// Seed the request from the stored row so a partial PATCH keeps the
	// fields it does not mention.
	req := taskRequest{
		Name: existing.Name, Description: existing.Description,
		CronExpr: existing.CronExpr, Timezone: existing.Timezone,
		Prompt: existing.Prompt, Model: existing.Model, Cwd: existing.Cwd,
		TimeoutSeconds: existing.TimeoutSeconds, MaxBudgetUSD: existing.MaxBudgetUSD,
		AllowedTools: existing.AllowedTools, Tools: existing.Tools,
		BypassPermissions: existing.BypassPermissions,
		OverlapPolicy:     existing.OverlapPolicy, GatingPolicy: existing.GatingPolicy,
	}
	for _, rq := range existing.Requirements {
		required := rq.Required
		req.Requirements = append(req.Requirements, requirementInput{
			Kind: rq.Kind, Target: rq.Target, Required: &required,
		})
	}

	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if problems := req.validate(); len(problems) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":    "validation failed",
			"problems": problems,
		})
		return
	}

	req.applyTo(existing)
	if err := s.store.UpdateTask(r.Context(), existing); err != nil {
		writeStoreError(w, err, "update task")
		return
	}

	s.reloadSchedules(r.Context())
	s.log.Info("task updated", "id", existing.ID, "name", existing.Name)
	writeJSON(w, http.StatusOK, s.reload(r, existing))
}

func (s *Server) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid task id")
		return
	}
	if err := s.store.DeleteTask(r.Context(), id); err != nil {
		writeStoreError(w, err, "delete task")
		return
	}
	s.reloadSchedules(r.Context())
	s.log.Info("task deleted", "id", id)
	w.WriteHeader(http.StatusNoContent)
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// reload re-reads a task so the response carries exactly what was persisted,
// including server-assigned requirement IDs and the computed next run.
// On failure the in-memory task is returned rather than erroring a write that
// already succeeded.
func (s *Server) reload(r *http.Request, fallback *store.Task) *store.Task {
	fresh, err := s.store.GetTask(r.Context(), fallback.ID)
	if err != nil {
		s.log.Warn("could not reload task after write", "id", fallback.ID, "error", err)
		return fallback
	}
	if next, err := nextRun(*fresh); err == nil && !next.IsZero() {
		fresh.NextRunAt = &next
	}
	fresh.Requirements = orEmpty(fresh.Requirements)
	fresh.LastExecutions = orEmpty(fresh.LastExecutions)
	return fresh
}
