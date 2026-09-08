// Package notify delivers scheduler events to the user.
//
// Everything goes through the Notifier interface so additional channels are a
// registration rather than a refactor. The v1 implementation is a desktop
// notification, which costs nothing to run; Slack is a webhook behind the
// same interface.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/raulsh/claude-scheduler/internal/store"
)

// Event kinds.
const (
	EventExecutionFailed      = "execution_failed"
	EventExecutionBlocked     = "execution_blocked"
	EventExecutionRateLimited = "execution_rate_limited"
	EventCheckDegraded        = "check_degraded"
	EventScheduleAutoPaused   = "schedule_auto_paused"
)

// Urgency levels, mapped onto each channel's own conventions.
type Urgency int

const (
	UrgencyLow Urgency = iota
	UrgencyNormal
	UrgencyCritical
)

// Event is something worth telling the user about.
type Event struct {
	Kind    string
	Title   string
	Body    string
	Urgency Urgency

	TaskID      *int64
	TaskName    string
	ExecutionID *int64
}

// Notifier delivers events over one channel.
type Notifier interface {
	// Name identifies the channel in logs and in the notifications table.
	Name() string
	// Notify delivers one event.
	Notify(ctx context.Context, ev Event) error
	// Enabled reports whether the channel is configured and usable.
	Enabled() bool
}

// Fanout delivers each event to every enabled channel and records the
// outcome. One channel failing never prevents the others from delivering.
type Fanout struct {
	store     *store.Store
	log       *slog.Logger
	notifiers []Notifier

	mu sync.Mutex
	// lastSent throttles repeats: a task failing every minute should not
	// produce a notification every minute.
	lastSent map[string]time.Time
	throttle time.Duration
}

// NewFanout builds a fanout over the given channels.
func NewFanout(st *store.Store, log *slog.Logger, notifiers ...Notifier) *Fanout {
	enabled := make([]Notifier, 0, len(notifiers))
	for _, n := range notifiers {
		if n != nil && n.Enabled() {
			enabled = append(enabled, n)
		}
	}
	return &Fanout{
		store:     st,
		log:       log,
		notifiers: enabled,
		lastSent:  make(map[string]time.Time),
		throttle:  5 * time.Minute,
	}
}

// Channels lists the enabled channel names.
func (f *Fanout) Channels() []string {
	names := make([]string, 0, len(f.notifiers))
	for _, n := range f.notifiers {
		names = append(names, n.Name())
	}
	return names
}

// Notify delivers an event, subject to throttling.
func (f *Fanout) Notify(ctx context.Context, ev Event) {
	if len(f.notifiers) == 0 {
		return
	}
	if f.throttled(ev) {
		f.log.Debug("notification throttled", "kind", ev.Kind, "task", ev.TaskName)
		return
	}

	// Detached so a cancelled request cannot suppress an alert about work
	// that already failed.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()

	for _, n := range f.notifiers {
		status, detail := "sent", ""
		if err := n.Notify(ctx, ev); err != nil {
			status, detail = "failed", err.Error()
			f.log.Warn("notification delivery failed",
				"channel", n.Name(), "kind", ev.Kind, "error", err)
		}
		if f.store != nil {
			if err := f.store.RecordNotification(ctx, ev.Kind, n.Name(), status, detail, ev.TaskID, ev.ExecutionID); err != nil {
				f.log.Debug("could not record notification", "error", err)
			}
		}
	}
}

// throttled reports whether an identical event was sent too recently.
func (f *Fanout) throttled(ev Event) bool {
	key := ev.Kind + "\x00" + ev.TaskName
	now := time.Now()

	f.mu.Lock()
	defer f.mu.Unlock()

	if last, seen := f.lastSent[key]; seen && now.Sub(last) < f.throttle {
		return true
	}
	f.lastSent[key] = now
	return false
}

// ForExecution builds the event for a finished execution, or reports false
// when the outcome is not worth interrupting the user for.
func ForExecution(task *store.Task, exec *store.Execution) (Event, bool) {
	ev := Event{
		TaskName:    taskName(task),
		ExecutionID: &exec.ID,
	}
	if task != nil {
		ev.TaskID = &task.ID
	}

	switch exec.Status {
	case store.StatusFailure, store.StatusTimeout:
		ev.Kind = EventExecutionFailed
		ev.Urgency = UrgencyCritical
		ev.Title = fmt.Sprintf("%s failed", ev.TaskName)
		ev.Body = firstNonEmpty(exec.ErrorMessage, "The run finished with an error.")

	case store.StatusBlocked:
		ev.Kind = EventExecutionBlocked
		ev.Urgency = UrgencyCritical
		ev.Title = fmt.Sprintf("%s was blocked", ev.TaskName)
		ev.Body = firstNonEmpty(exec.ErrorMessage, "A dependency was not healthy, so the run was skipped.")

	case store.StatusRateLimited:
		// Not a defect in the task, and phrased so it does not read like one.
		ev.Kind = EventExecutionRateLimited
		ev.Urgency = UrgencyNormal
		ev.Title = fmt.Sprintf("%s hit a usage limit", ev.TaskName)
		ev.Body = firstNonEmpty(exec.ErrorMessage, "The run stopped because a usage limit was reached.")

	default:
		// Success, cancelled and skipped are not interruptions.
		return Event{}, false
	}

	ev.Body = truncate(ev.Body, 300)
	return ev, true
}

// ForAutoPause builds the event for a schedule that paused itself.
func ForAutoPause(task *store.Task, reason string) Event {
	name := taskName(task)
	ev := Event{
		Kind:     EventScheduleAutoPaused,
		Urgency:  UrgencyCritical,
		TaskName: name,
		Title:    fmt.Sprintf("%s is paused", name),
		Body: truncate("This schedule paused itself and will not run again until you fix it: "+
			reason, 300),
	}
	if task != nil {
		ev.TaskID = &task.ID
	}
	return ev
}

func taskName(task *store.Task) string {
	if task == nil || task.Name == "" {
		return "A scheduled task"
	}
	return task.Name
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
