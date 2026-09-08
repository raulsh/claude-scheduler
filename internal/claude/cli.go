package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrBinaryNotFound reports that the claude CLI could not be located.
var ErrBinaryNotFound = errors.New("the claude CLI was not found")

// CLI describes how to invoke the claude binary.
type CLI struct {
	// Path is the configured binary location. It is commonly a version
	// symlink under ~/.local/bin that moves when the CLI self-updates.
	Path string
}

// Locate finds the claude binary, preferring an explicit path.
func Locate(configured string) (CLI, error) {
	if configured != "" {
		if err := checkExecutable(configured); err != nil {
			return CLI{}, err
		}
		return CLI{Path: configured}, nil
	}

	if p, err := exec.LookPath("claude"); err == nil {
		return CLI{Path: p}, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".local", "bin", "claude")
		if checkExecutable(candidate) == nil {
			return CLI{Path: candidate}, nil
		}
	}
	return CLI{}, ErrBinaryNotFound
}

func checkExecutable(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%w at %s: %w", ErrBinaryNotFound, path, err)
	}
	if st.IsDir() {
		return fmt.Errorf("%w: %s is a directory", ErrBinaryNotFound, path)
	}
	if st.Mode()&0o111 == 0 {
		return fmt.Errorf("%w: %s is not executable", ErrBinaryNotFound, path)
	}
	return nil
}

// Resolved returns the real path behind any symlinks. The CLI installs as a
// symlink to a versioned binary and self-updates by moving it, so resolving
// per launch records what actually ran.
func (c CLI) Resolved() string {
	if resolved, err := filepath.EvalSymlinks(c.Path); err == nil {
		return resolved
	}
	return c.Path
}

// Version queries the CLI's own version string.
func (c CLI) Version(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, c.Path, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("claude --version: %w", err)
	}
	// Output looks like "2.1.263 (Claude Code)".
	version, _, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	return version, nil
}

// RunOptions describes one scheduled invocation.
type RunOptions struct {
	// Prompt is delivered on stdin rather than as an argument, which avoids
	// both ARG_MAX limits and any shell-quoting concerns for long prompts.
	Prompt string

	Model     string
	SessionID string
	Cwd       string

	// AllowedTools are permission rules such as `Bash(aws *)` or `Read`.
	// These pre-approve actions that would otherwise prompt; they do not
	// restrict what the model can reach for.
	AllowedTools []string

	// Tools restricts the built-in tool set outright. This is the only hard
	// limit of the three permission-related options: an allowlist plus
	// --permission-prompts none still lets the CLI auto-approve actions it
	// judges harmless, so a task that must not run commands has to have the
	// tool removed rather than merely un-allowlisted.
	//
	// Nil means the CLI default set. A non-nil empty slice disables every
	// built-in tool.
	Tools []string

	// RestrictToolsToNone requests the empty tool set explicitly, since a
	// nil and an empty slice cannot otherwise be told apart after a round
	// trip through JSON.
	RestrictToolsToNone bool

	// BypassPermissions maps to --dangerously-skip-permissions. When false
	// the run uses --permission-prompts none, so an unallowlisted tool is
	// denied immediately instead of blocking forever on a prompt nobody can
	// answer.
	BypassPermissions bool

	MaxBudgetUSD float64
	Timeout      time.Duration

	// AppendSystemPrompt is extra system-prompt text for the run. It appends
	// to the CLI's own system prompt rather than replacing it, so a run keeps
	// everything the CLI normally tells the model and gains only what the
	// scheduler has to add. Empty means the CLI's system prompt, untouched.
	AppendSystemPrompt string

	// ExtraMCPConfig is an optional --mcp-config JSON payload.
	ExtraMCPConfig string
}

// Args builds the argument list for a run.
func (c CLI) Args(opts RunOptions) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
	}

	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.SessionID != "" {
		args = append(args, "--session-id", opts.SessionID)
	}
	if opts.Cwd != "" {
		args = append(args, "--add-dir", opts.Cwd)
	}

	if opts.BypassPermissions {
		args = append(args, "--dangerously-skip-permissions")
	} else {
		// Nobody is present to answer a prompt on a scheduled run, so
		// anything not covered by the allowlist is denied rather than left
		// hanging. The denials come back in the result event.
		args = append(args, "--permission-prompts", "none")
		if rules := joinToolRules(opts.AllowedTools); rules != "" {
			// A single comma-separated value, not one argument per rule: the
			// flag is variadic in the CLI and would otherwise swallow the
			// flags that follow it.
			args = append(args, "--allowedTools", rules)
		}
	}

	switch {
	case opts.RestrictToolsToNone:
		// An explicit empty value disables every built-in tool.
		args = append(args, "--tools", "")
	case len(opts.Tools) > 0:
		args = append(args, "--tools", strings.Join(opts.Tools, ","))
	}

	if opts.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", opts.AppendSystemPrompt)
	}
	if opts.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(opts.MaxBudgetUSD, 'f', -1, 64))
	}
	if opts.ExtraMCPConfig != "" {
		args = append(args, "--mcp-config", opts.ExtraMCPConfig)
	}

	return args
}

// joinToolRules renders allowlist rules as one comma-separated value,
// dropping blanks. Rules themselves contain spaces (`Bash(aws *)`), so
// comma is the only safe separator within a single argument.
func joinToolRules(rules []string) string {
	cleaned := make([]string, 0, len(rules))
	for _, r := range rules {
		if r = strings.TrimSpace(r); r != "" {
			cleaned = append(cleaned, r)
		}
	}
	return strings.Join(cleaned, ",")
}
