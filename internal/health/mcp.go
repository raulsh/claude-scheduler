package health

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/raulsh/claude-scheduler/internal/store"
)

// MCPChecker reports the status of the MCP servers the claude CLI knows about.
//
// The CLI is the only source of truth here. Reading ~/.claude.json is not
// enough: on a typical setup it holds only the locally-configured servers,
// while the account-level claude.ai connectors - the large majority - are
// resolved server-side and appear nowhere in that file.
type MCPChecker struct {
	// Binary is the claude CLI path. Empty means it was not found.
	Binary string

	Timeout  time.Duration
	CacheFor time.Duration

	// The probe costs 10-60s of network health checks, so its result is
	// memoised here as well as in the registry. Without this, enumerating
	// targets and then checking one would run it twice.
	mu     sync.Mutex
	memo   map[string]Result
	memoAt time.Time
}

// Kind implements Checker.
func (c *MCPChecker) Kind() string { return store.KindMCPServer }

// TTL implements Checker. The probe health-checks every server over the
// network and can take a minute, so its result is cached rather than re-run
// per execution.
func (c *MCPChecker) TTL() time.Duration {
	if c.CacheFor > 0 {
		return c.CacheFor
	}
	return 2 * time.Minute
}

func (c *MCPChecker) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 90 * time.Second
}

// mcpLine matches one server row of `claude mcp list`, for example:
//
// The punctuation in these samples, including the em-dash before a failure
// reason, is reproduced from the CLI's real output rather than chosen.
//
//	lemn-signals: https://mcp.lemn.sh/signals (HTTP) - ✔ Connected
//	claude.ai Slack: https://mcp.slack.com/mcp - ! Needs authentication
//	claude-design: https://x/mcp (HTTP) - ✘ Failed to connect — <reason>
//
// The name group is non-greedy so it stops at the first ": ", which a URL's
// "://" cannot be mistaken for. The detail group is non-greedy so the first
// " - <marker> " separator wins, leaving any error prose intact afterwards.
var mcpLine = regexp.MustCompile(`^(.+?): (.*?) - ([✔✘⏸!]) (.+)$`)

// CheckAll probes every configured MCP server in one invocation.
//
// Note that `claude mcp list` exits 0 even when servers are unauthenticated
// or unreachable, and writes everything to stdout. The exit code carries no
// health information, so the status markers are the only signal.
func (c *MCPChecker) CheckAll(ctx context.Context) (map[string]Result, error) {
	if c.Binary == "" {
		return nil, errors.New("the claude CLI was not found on this system")
	}

	c.mu.Lock()
	if c.memo != nil && time.Since(c.memoAt) < c.TTL() {
		cached := c.memo
		c.mu.Unlock()
		return cached, nil
	}
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(ctx, c.Binary, "mcp", "list")
	cmd.Env = os.Environ()

	out, err := cmd.Output()
	latency := time.Since(start)

	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("claude mcp list timed out after %s", c.timeout())
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("claude mcp list: %s", firstErrorLine(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("claude mcp list: %w", err)
	}

	results := parseMCPList(string(out), latency)

	c.mu.Lock()
	c.memo, c.memoAt = results, time.Now()
	c.mu.Unlock()

	if len(results) == 0 {
		// No servers configured is a legitimate state; an unparseable
		// output is not, and the two look the same here. Report empty and
		// let the caller decide, rather than inventing a failure.
		return map[string]Result{}, nil
	}
	return results, nil
}

// parseMCPList turns the CLI's human-readable output into results.
func parseMCPList(out string, latency time.Duration) map[string]Result {
	now := time.Now()
	results := make(map[string]Result)

	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Checking MCP server health") {
			continue
		}

		m := mcpLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name, endpoint, marker, statusText := m[1], m[2], m[3], strings.TrimSpace(m[4])

		state, detail := mcpState(marker, statusText, endpoint)
		res := Result{
			Kind:      store.KindMCPServer,
			Target:    name,
			State:     state,
			Detail:    detail,
			CheckedAt: now,
			Latency:   latency,
			LatencyMS: latency.Milliseconds(),
		}
		if state == store.CheckNeedsLogin {
			res.Remediation = &Remediation{
				Kind: "manual_command",
				// The MCP login flow requires pasting a redirect URL back
				// into the terminal, so the service cannot drive it.
				Command:   fmt.Sprintf("claude mcp login %q", name),
				Automatic: false,
				Hint: "Run this in a terminal. Unlike the AWS SSO flow, MCP login " +
					"needs the redirect URL pasted back, so it cannot be completed " +
					"from this page.",
			}
		}
		results[name] = res
	}
	return results
}

// mcpState maps a status marker onto a check state.
func mcpState(marker, statusText, endpoint string) (State, string) {
	switch marker {
	case "✔":
		return store.CheckOK, describeEndpoint(statusText, endpoint)
	case "!":
		// "Needs authentication": a human must complete an OAuth flow.
		return store.CheckNeedsLogin, statusText
	case "⏸":
		// "Pending approval": a project-scoped server the user has not yet
		// accepted. Real, and a task depending on it cannot work.
		return store.CheckMisconfigured, statusText
	case "✘":
		// "Failed to connect": treated as transient so a server having a bad
		// day cannot pause every schedule that mentions it.
		return store.CheckUnavailable, statusText
	default:
		return store.CheckUnknown, statusText
	}
}

func describeEndpoint(statusText, endpoint string) string {
	if endpoint == "" {
		return statusText
	}
	return statusText + " · " + endpoint
}

// Check probes one server. It delegates to the bulk probe, which the
// registry prefers anyway, so a single lookup still warms every sibling.
func (c *MCPChecker) Check(ctx context.Context, target string) Result {
	all, err := c.CheckAll(ctx)
	if err != nil {
		return Result{
			Kind: c.Kind(), Target: target, State: store.CheckUnavailable,
			Detail: err.Error(), CheckedAt: time.Now(),
		}
	}
	if res, ok := all[target]; ok {
		return res
	}
	return Result{
		Kind: c.Kind(), Target: target, State: store.CheckMisconfigured,
		Detail:    fmt.Sprintf("MCP server %q is not configured", target),
		CheckedAt: time.Now(),
	}
}

// Targets lists every MCP server the CLI reports.
func (c *MCPChecker) Targets(ctx context.Context) ([]string, error) {
	all, err := c.CheckAll(ctx)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}
