// Package scheduler fires tasks on their cron schedules.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/raulsh/claude-scheduler/internal/config"
	"github.com/raulsh/claude-scheduler/internal/schedule"
	"github.com/raulsh/claude-scheduler/internal/store"
)

// Trigger starts a run. The executor satisfies this; keeping it an interface
// makes the scheduler testable without spawning processes.
type Trigger interface {
	Trigger(ctx context.Context, task *store.Task, trigger string) (*store.Execution, error)
}

// Scheduler owns the cron registrations for all enabled tasks.
type Scheduler struct {
	cfg     config.Config
	store   *store.Store
	trigger Trigger
	log     *slog.Logger

	mu      sync.Mutex
	cron    *cron.Cron
	entries map[int64]cron.EntryID
	specs   map[int64]string
	started bool
}

// New creates a scheduler.
func New(cfg config.Config, st *store.Store, trigger Trigger, log *slog.Logger) *Scheduler {
	return &Scheduler{
		cfg:     cfg,
		store:   st,
		trigger: trigger,
		log:     log,
		cron:    cron.New(),
		entries: make(map[int64]cron.EntryID),
		specs:   make(map[int64]string),
	}
}

// Start loads tasks, handles any missed occurrences and begins ticking.
func (s *Scheduler) Start(ctx context.Context) error {
	if err := s.Reload(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	s.cron.Start()
	s.started = true
	s.mu.Unlock()

	s.log.Info("scheduler started", "tasks", s.Count())

	// Missed occurrences are handled after ticking begins, so a slow
	// catch-up cannot delay normal scheduling.
	go s.catchUp(ctx)
	return nil
}

// Stop halts the scheduler, waiting for any running jobs to be dispatched.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return
	}
	<-s.cron.Stop().Done()
	s.started = false
}

// Count reports how many tasks are registered.
func (s *Scheduler) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Reload synchronises registrations with the database. It is called at
// startup and after any task mutation, and only touches entries whose
// schedule actually changed so unrelated timers are not reset.
func (s *Scheduler) Reload(ctx context.Context) error {
	tasks, err := s.store.ListTasks(ctx)
	if err != nil {
		return fmt.Errorf("load tasks for scheduling: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	wanted := make(map[int64]store.Task, len(tasks))
	for _, t := range tasks {
		// A paused or disabled task stays registered nowhere, so it cannot
		// fire until it is explicitly resumed.
		if t.Enabled && !t.Paused {
			wanted[t.ID] = t
		}
	}

	// Drop registrations that are no longer wanted or whose spec changed.
	for id, entryID := range s.entries {
		task, keep := wanted[id]
		if keep && s.specs[id] == specKey(task) {
			continue
		}
		s.cron.Remove(entryID)
		delete(s.entries, id)
		delete(s.specs, id)
	}

	// Add anything missing.
	for id, task := range wanted {
		if _, exists := s.entries[id]; exists {
			continue
		}

		sched, err := schedule.Parse(task.CronExpr, task.Timezone)
		if err != nil {
			// A task with an unparseable schedule is skipped rather than
			// failing the whole reload; the API validates on write, so this
			// only happens if the row was edited outside the service.
			s.log.Error("skipping task with an invalid schedule",
				"task", task.Name, "cron", task.CronExpr, "error", err)
			continue
		}

		// schedule.Schedule satisfies cron.Schedule, so timezone handling
		// stays in one place and matches the next-run times shown in the UI.
		entryID := s.cron.Schedule(sched, s.jobFor(id, task.Name))
		s.entries[id] = entryID
		s.specs[id] = specKey(task)
	}

	return nil
}

// specKey changes whenever something scheduling-relevant changes, so Reload
// knows to re-register.
func specKey(t store.Task) string { return t.CronExpr + "\x00" + t.Timezone }

// jobFor builds the cron job for a task. The task is re-read at fire time so
// a run always uses the current definition, not a stale copy.
func (s *Scheduler) jobFor(taskID int64, name string) cron.Job {
	return cron.FuncJob(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		task, err := s.store.GetTask(ctx, taskID)
		if err != nil {
			s.log.Error("could not load task at fire time", "task", name, "id", taskID, "error", err)
			return
		}
		if !task.Enabled || task.Paused {
			// It changed between registration and firing.
			return
		}

		exec, err := s.trigger.Trigger(ctx, task, store.TriggerCron)
		if err != nil {
			s.log.Error("could not start scheduled run", "task", task.Name, "error", err)
			return
		}
		s.log.Info("scheduled run started",
			"task", task.Name, "execution", exec.ID, "status", exec.Status)
	})
}

// NextRuns reports the next fire time for every registered task.
func (s *Scheduler) NextRuns() map[int64]time.Time {
	s.mu.Lock()
	entries := make(map[int64]cron.EntryID, len(s.entries))
	for id, entryID := range s.entries {
		entries[id] = entryID
	}
	cronRef := s.cron
	s.mu.Unlock()

	out := make(map[int64]time.Time, len(entries))
	for id, entryID := range entries {
		if entry := cronRef.Entry(entryID); entry.ID != 0 {
			out[id] = entry.Next
		}
	}
	return out
}

// catchUp runs at most one missed occurrence per task after downtime.
//
// Replaying a full backlog would be actively harmful here: a task that fires
// every 15 minutes and was down overnight would launch dozens of Claude runs
// at once, spending real money. Running only the most recent missed
// occurrence, and only inside the grace window, keeps restarts safe.
func (s *Scheduler) catchUp(ctx context.Context) {
	grace := s.cfg.Executor.MisfireGrace.Std()
	if grace <= 0 {
		s.log.Debug("catch-up disabled by configuration")
		return
	}

	tasks, err := s.store.ListTasks(ctx)
	if err != nil {
		s.log.Warn("could not evaluate missed runs", "error", err)
		return
	}

	now := time.Now()
	for _, task := range tasks {
		if !task.Enabled || task.Paused {
			continue
		}
		select {
		case <-ctx.Done():
			return
		default:
		}

		sched, err := schedule.Parse(task.CronExpr, task.Timezone)
		if err != nil {
			continue
		}

		last, err := s.lastRunAt(ctx, task.ID)
		if err != nil {
			s.log.Warn("could not read last run", "task", task.Name, "error", err)
			continue
		}
		if last.IsZero() {
			// Never run before: there is nothing to catch up on, and firing
			// immediately would surprise the user who just created it.
			continue
		}

		missed, found := sched.MissedSince(last, now)
		if !found {
			continue
		}
		if age := now.Sub(missed); age > grace {
			s.log.Info("skipping a missed run that is outside the grace window",
				"task", task.Name, "missed_at", missed, "age", age.Round(time.Second), "grace", grace)
			continue
		}

		s.log.Info("running one missed occurrence after downtime",
			"task", task.Name, "missed_at", missed)

		taskCopy := task
		exec, err := s.trigger.Trigger(ctx, &taskCopy, store.TriggerCron)
		if err != nil {
			s.log.Error("could not run missed occurrence", "task", task.Name, "error", err)
			continue
		}
		s.log.Info("missed occurrence dispatched",
			"task", task.Name, "execution", exec.ID, "status", exec.Status)
	}
}

// lastRunAt reports when a task most recently ran, zero if never.
func (s *Scheduler) lastRunAt(ctx context.Context, taskID int64) (time.Time, error) {
	execs, _, err := s.store.ListExecutions(ctx, store.ExecutionFilter{
		TaskID: &taskID,
		Limit:  1,
	})
	if err != nil {
		return time.Time{}, err
	}
	if len(execs) == 0 {
		return time.Time{}, nil
	}
	return execs[0].QueuedAt, nil
}
