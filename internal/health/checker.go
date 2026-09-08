// Package health checks the dependencies a scheduled task declares, so a run
// never executes against an expired credential or an unreachable service.
//
// The distinction that matters throughout is between a dependency that needs
// human attention (NeedsLogin, Misconfigured) and one that is merely
// unreachable right now (Unavailable). Only the former gates an execution or
// pauses a schedule: a flaky network must not look like a dead credential.
package health

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/raulsh/claude-scheduler/internal/store"
)

// State classifies a dependency's condition.
type State = string

// Result is one dependency's condition at a point in time.
type Result struct {
	Kind      string        `json:"kind"`
	Target    string        `json:"target"`
	State     State         `json:"state"`
	Detail    string        `json:"detail"`
	CheckedAt time.Time     `json:"checked_at"`
	Latency   time.Duration `json:"-"`
	LatencyMS int64         `json:"latency_ms"`

	// Remediation describes how to fix the dependency, when the service can
	// help. Nil when the only route is manual.
	Remediation *Remediation `json:"remediation,omitempty"`
}

// Gates reports whether this result should stop an execution.
func (r Result) Gates() bool { return store.Gates(r.State) }

// Remediation tells the UI what can be done about a failing dependency.
type Remediation struct {
	// Kind is "aws_sso_login" for a flow the service can drive itself, or
	// "manual_command" when the user must run something interactively.
	Kind string `json:"kind"`

	// Command is the exact command to run, shown with a copy button for the
	// manual case.
	Command string `json:"command,omitempty"`

	// Automatic reports whether the service can perform this itself.
	Automatic bool `json:"automatic"`

	// Hint explains what the user should expect.
	Hint string `json:"hint,omitempty"`
}

// Checker probes one class of dependency.
type Checker interface {
	// Kind identifies the dependency class, matching a task requirement kind.
	Kind() string

	// Check probes a single target.
	Check(ctx context.Context, target string) Result

	// Targets enumerates everything discoverable for this kind, so the health
	// page can show dependencies the user has not referenced yet.
	Targets(ctx context.Context) ([]string, error)

	// TTL is how long a result stays fresh.
	TTL() time.Duration
}

// BulkChecker is implemented by checkers that can probe every target in one
// operation. `claude mcp list` health-checks all servers at once and takes
// up to a minute, so probing targets individually would be far slower.
type BulkChecker interface {
	Checker
	CheckAll(ctx context.Context) (map[string]Result, error)
}

// Registry caches results and dispatches to the right checker.
type Registry struct {
	log      *slog.Logger
	store    *store.Store
	checkers map[string]Checker

	mu    sync.Mutex
	cache map[string]cached
	// inflight collapses concurrent probes of the same target into one.
	inflight map[string]*flight
}

type cached struct {
	result Result
	expiry time.Time
}

type flight struct {
	done   chan struct{}
	result Result
}

// NewRegistry builds a registry over the given checkers.
func NewRegistry(st *store.Store, log *slog.Logger, checkers ...Checker) *Registry {
	byKind := make(map[string]Checker, len(checkers))
	for _, c := range checkers {
		byKind[c.Kind()] = c
	}
	return &Registry{
		log:      log,
		store:    st,
		checkers: byKind,
		cache:    make(map[string]cached),
		inflight: make(map[string]*flight),
	}
}

// Kinds lists the registered dependency kinds.
func (r *Registry) Kinds() []string {
	out := make([]string, 0, len(r.checkers))
	for k := range r.checkers {
		out = append(out, k)
	}
	return out
}

// Checker returns the checker for a kind.
func (r *Registry) Checker(kind string) (Checker, bool) {
	c, ok := r.checkers[kind]
	return c, ok
}

func cacheKey(kind, target string) string { return kind + "\x00" + target }

// Check returns a dependency's condition, using the cache unless force is set.
func (r *Registry) Check(ctx context.Context, kind, target string, force bool) Result {
	checker, ok := r.checkers[kind]
	if !ok {
		return Result{
			Kind: kind, Target: target, State: store.CheckUnknown,
			Detail:    fmt.Sprintf("no checker is registered for %q", kind),
			CheckedAt: time.Now(),
		}
	}

	key := cacheKey(kind, target)

	if !force {
		r.mu.Lock()
		entry, hit := r.cache[key]
		r.mu.Unlock()
		if hit && time.Now().Before(entry.expiry) {
			return entry.result
		}
	}

	// Collapse concurrent probes: an MCP check takes up to a minute, and a
	// page load asking about ten servers must not trigger ten of them.
	r.mu.Lock()
	if f, running := r.inflight[key]; running {
		r.mu.Unlock()
		select {
		case <-f.done:
			return f.result
		case <-ctx.Done():
			return Result{
				Kind: kind, Target: target, State: store.CheckUnknown,
				Detail:    "the check was still running when the request was cancelled",
				CheckedAt: time.Now(),
			}
		}
	}
	f := &flight{done: make(chan struct{})}
	r.inflight[key] = f
	r.mu.Unlock()

	result := r.probe(ctx, checker, kind, target)

	r.mu.Lock()
	f.result = result
	close(f.done)
	delete(r.inflight, key)
	r.cache[key] = cached{result: result, expiry: time.Now().Add(checker.TTL())}
	r.mu.Unlock()

	r.persist(ctx, result, nil)
	return result
}

// probe runs a single check, preferring a bulk probe when the checker offers
// one, since that also warms the cache for sibling targets.
func (r *Registry) probe(ctx context.Context, checker Checker, kind, target string) Result {
	bulk, ok := checker.(BulkChecker)
	if !ok {
		return timed(ctx, checker, target)
	}

	all, err := bulk.CheckAll(ctx)
	if err != nil {
		return Result{
			Kind: kind, Target: target, State: store.CheckUnavailable,
			Detail: err.Error(), CheckedAt: time.Now(),
		}
	}

	// Warm the siblings: one expensive probe answered every target.
	now := time.Now()
	expiry := now.Add(checker.TTL())
	r.mu.Lock()
	for name, res := range all {
		if name != target {
			r.cache[cacheKey(kind, name)] = cached{result: res, expiry: expiry}
		}
	}
	r.mu.Unlock()

	if res, found := all[target]; found {
		return res
	}
	return Result{
		Kind: kind, Target: target, State: store.CheckMisconfigured,
		Detail:    fmt.Sprintf("%q is not configured", target),
		CheckedAt: now,
	}
}

func timed(ctx context.Context, checker Checker, target string) Result {
	start := time.Now()
	res := checker.Check(ctx, target)
	if res.CheckedAt.IsZero() {
		res.CheckedAt = time.Now()
	}
	if res.Latency == 0 {
		res.Latency = time.Since(start)
	}
	res.LatencyMS = res.Latency.Milliseconds()
	return res
}

// CheckRequirements probes every requirement of a task and returns the
// results together with the subset that gates execution.
func (r *Registry) CheckRequirements(ctx context.Context, reqs []store.Requirement, force bool) (all, failing []Result) {
	for _, req := range reqs {
		res := r.Check(ctx, req.Kind, req.Target, force)
		all = append(all, res)

		// An optional requirement is reported but never blocks.
		if req.Required && res.Gates() {
			failing = append(failing, res)
		}
	}
	return all, failing
}

// persist records a result for history and for the health page.
func (r *Registry) persist(ctx context.Context, res Result, executionID *int64) {
	if r.store == nil {
		return
	}
	// Use a detached context: a cancelled request should not discard a
	// result that was already obtained.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	err := r.store.SaveCheckResult(ctx, &store.CheckResult{
		ExecutionID: executionID,
		Kind:        res.Kind,
		Target:      res.Target,
		State:       res.State,
		Detail:      res.Detail,
		LatencyMS:   res.Latency.Milliseconds(),
		CheckedAt:   res.CheckedAt,
	})
	if err != nil {
		r.log.Warn("could not persist check result",
			"kind", res.Kind, "target", res.Target, "error", err)
	}
}

// Snapshot probes every discoverable target of every kind, for the health
// page. Errors enumerating one kind do not stop the others.
func (r *Registry) Snapshot(ctx context.Context, force bool) map[string][]Result {
	out := make(map[string][]Result, len(r.checkers))

	for kind, checker := range r.checkers {
		targets, err := checker.Targets(ctx)
		if err != nil {
			r.log.Warn("could not enumerate check targets", "kind", kind, "error", err)
			out[kind] = []Result{{
				Kind: kind, State: store.CheckUnavailable,
				Detail: err.Error(), CheckedAt: time.Now(),
			}}
			continue
		}

		results := make([]Result, 0, len(targets))
		for _, target := range targets {
			results = append(results, r.Check(ctx, kind, target, force))
		}
		out[kind] = results
	}
	return out
}

// Invalidate drops a cached result, so the next check re-probes. Called after
// a remediation completes.
func (r *Registry) Invalidate(kind, target string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, cacheKey(kind, target))
}

// InvalidateKind drops every cached result for a kind.
func (r *Registry) InvalidateKind(kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.cache {
		if k, _, found := cut(key); found && k == kind {
			delete(r.cache, key)
		}
	}
}

func cut(key string) (kind, target string, found bool) {
	for i := range len(key) {
		if key[i] == 0 {
			return key[:i], key[i+1:], true
		}
	}
	return key, "", false
}

// checkConcurrency bounds how many probes run at once. Each probe is a
// subprocess, so some parallelism is a large win: eleven AWS profiles at
// roughly a second each take twelve seconds in sequence. An unbounded
// fan-out would spawn a process per configured target instead.
const checkConcurrency = 6

// CheckAll probes many targets of one kind concurrently, preserving the
// order of the targets given.
//
// A checker that implements BulkChecker answers every target from a single
// operation, so the fan-out is skipped entirely.
func (r *Registry) CheckAll(ctx context.Context, kind string, targets []string, force bool) []Result {
	results := make([]Result, len(targets))
	if len(targets) == 0 {
		return results
	}

	// One probe answers everything, and the registry's cache warming makes
	// the remaining lookups free.
	if _, bulk := r.checkers[kind].(BulkChecker); bulk {
		for i, target := range targets {
			results[i] = r.Check(ctx, kind, target, force && i == 0)
		}
		return results
	}

	var wg sync.WaitGroup
	slots := make(chan struct{}, checkConcurrency)

	for i, target := range targets {
		wg.Add(1)
		go func(i int, target string) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			results[i] = r.Check(ctx, kind, target, force)
		}(i, target)
	}
	wg.Wait()

	return results
}
