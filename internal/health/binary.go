package health

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/raulsh/claude-scheduler/internal/store"
)

// BinaryChecker verifies that a command a task depends on exists and runs.
type BinaryChecker struct {
	// Probes maps a target name to how it should be verified. A target with
	// no entry is checked for presence on PATH only.
	Probes map[string]BinaryProbe

	Timeout  time.Duration
	CacheFor time.Duration
}

// BinaryProbe describes how to verify one binary.
type BinaryProbe struct {
	// Path pins the binary location. Empty means look it up on PATH.
	Path string
	// VersionArgs runs a cheap liveness check, conventionally --version.
	// Empty skips execution and checks only that the file is present.
	VersionArgs []string
}

// Kind implements Checker.
func (c *BinaryChecker) Kind() string { return store.KindBinary }

// TTL implements Checker.
func (c *BinaryChecker) TTL() time.Duration {
	if c.CacheFor > 0 {
		return c.CacheFor
	}
	return 10 * time.Minute
}

func (c *BinaryChecker) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 15 * time.Second
}

// Check verifies one binary.
func (c *BinaryChecker) Check(ctx context.Context, target string) Result {
	start := time.Now()
	res := Result{Kind: c.Kind(), Target: target, CheckedAt: start}

	probe := c.Probes[target]
	path := probe.Path
	if path == "" {
		resolved, err := exec.LookPath(target)
		if err != nil {
			res.State = store.CheckMisconfigured
			res.Detail = fmt.Sprintf("%q was not found on PATH", target)
			res.Latency = time.Since(start)
			res.Remediation = &Remediation{
				Kind:      "manual_command",
				Automatic: false,
				Hint:      fmt.Sprintf("Install %s, or set its path in the scheduler configuration.", target),
			}
			return res
		}
		path = resolved
	}

	args := probe.VersionArgs
	if len(args) == 0 {
		args = []string{"--version"}
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	res.Latency = time.Since(start)
	res.LatencyMS = res.Latency.Milliseconds()

	if err != nil {
		if ctx.Err() != nil {
			res.State = store.CheckUnavailable
			res.Detail = fmt.Sprintf("%s %s timed out", path, strings.Join(args, " "))
			return res
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// The binary exists but its version probe failed. That is odd
			// enough to surface, but it is not a credential problem.
			res.State = store.CheckUnknown
			res.Detail = fmt.Sprintf("%s exited %d for %s", path, ee.ExitCode(), strings.Join(args, " "))
			return res
		}
		res.State = store.CheckUnavailable
		res.Detail = err.Error()
		return res
	}

	res.State = store.CheckOK
	res.Detail = firstLine(string(out))
	if res.Detail == "" {
		res.Detail = path
	} else {
		res.Detail += " · " + path
	}
	return res
}

// Targets lists the binaries with configured probes.
func (c *BinaryChecker) Targets(ctx context.Context) ([]string, error) {
	names := make([]string, 0, len(c.Probes))
	for name := range c.Probes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}
