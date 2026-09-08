// Package schedule parses cron expressions with an explicit timezone and
// answers "when does this fire next".
//
// Timezone handling lives here rather than in the scheduler or the API so
// that the next-run times shown in the UI are computed by exactly the same
// code that decides when a task actually fires.
package schedule

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// parser accepts standard five-field cron plus the @every and @daily style
// descriptors. Seconds are deliberately not enabled: a scheduler that shells
// out to an LLM has no business firing sub-minute.
var parser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// Schedule is a parsed cron expression bound to a location.
type Schedule struct {
	expr     string
	location *time.Location
	inner    cron.Schedule
}

// Parse validates a cron expression against a timezone name. An empty or
// "Local" timezone resolves to the service's local time.
func Parse(expr, timezone string) (Schedule, error) {
	loc, err := LoadLocation(timezone)
	if err != nil {
		return Schedule{}, err
	}

	inner, err := parser.Parse(expr)
	if err != nil {
		return Schedule{}, fmt.Errorf("invalid cron expression %q: %w", expr, err)
	}
	return Schedule{expr: expr, location: loc, inner: inner}, nil
}

// LoadLocation resolves a timezone name, treating empty and "Local" alike.
func LoadLocation(timezone string) (*time.Location, error) {
	if timezone == "" || timezone == "Local" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q: %w", timezone, err)
	}
	return loc, nil
}

// Next returns the first fire time strictly after t.
func (s Schedule) Next(t time.Time) time.Time {
	if s.inner == nil {
		return time.Time{}
	}
	return s.inner.Next(t.In(s.location))
}

// NextN returns the next n fire times after t, which is what the task form
// previews so the user can confirm a cron expression means what they think.
func (s Schedule) NextN(t time.Time, n int) []time.Time {
	if s.inner == nil || n <= 0 {
		return nil
	}
	out := make([]time.Time, 0, n)
	cursor := t
	for range n {
		cursor = s.Next(cursor)
		if cursor.IsZero() {
			break
		}
		out = append(out, cursor)
	}
	return out
}

// Location reports the timezone the schedule is evaluated in.
func (s Schedule) Location() *time.Location { return s.location }

// Expr returns the original expression.
func (s Schedule) Expr() string { return s.expr }

// MissedSince reports the most recent fire time in (since, now], and whether
// one exists. The scheduler uses this to decide whether to run a single
// catch-up occurrence after downtime, rather than replaying a whole backlog.
func (s Schedule) MissedSince(since, now time.Time) (time.Time, bool) {
	if s.inner == nil || since.IsZero() || !since.Before(now) {
		return time.Time{}, false
	}

	var last time.Time
	cursor := since
	// Bounded so a wide gap with a frequent schedule cannot spin.
	for range 100000 {
		next := s.Next(cursor)
		if next.IsZero() || next.After(now) {
			break
		}
		last = next
		cursor = next
	}
	return last, !last.IsZero()
}
