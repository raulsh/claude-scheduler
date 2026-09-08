package health

import (
	"context"
	"time"

	"github.com/raulsh/claude-scheduler/internal/store"
)

// Preflight returns an evaluator suitable for the executor, bound to this
// registry.
//
// Results come from the TTL cache where they are fresh. That is deliberate:
// the MCP probe health-checks every server over the network and takes up to
// a minute, so re-running it for every execution would dominate the run.
func (r *Registry) Preflight() func(ctx context.Context, task *store.Task) (all, failing []store.CheckResult) {
	return func(ctx context.Context, task *store.Task) (all, failing []store.CheckResult) {
		results, failed := r.CheckRequirements(ctx, task.Requirements, false)

		all = make([]store.CheckResult, 0, len(results))
		for _, res := range results {
			all = append(all, res.toStore())
		}
		failing = make([]store.CheckResult, 0, len(failed))
		for _, res := range failed {
			failing = append(failing, res.toStore())
		}
		return all, failing
	}
}

// toStore converts a check result into its persisted form.
func (r Result) toStore() store.CheckResult {
	checkedAt := r.CheckedAt
	if checkedAt.IsZero() {
		checkedAt = time.Now()
	}
	return store.CheckResult{
		Kind:      r.Kind,
		Target:    r.Target,
		State:     r.State,
		Detail:    r.Detail,
		LatencyMS: r.Latency.Milliseconds(),
		CheckedAt: checkedAt,
	}
}
