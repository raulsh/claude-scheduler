package cli

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/raulsh/claude-scheduler/internal/config"
)

// Usage writes the client command summary. The daemon's own commands are
// listed by main, which owns them.
func Usage(w io.Writer) {
	fmt.Fprint(w, `Client commands, all of which talk to a running daemon over its socket:

`)
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	for _, c := range topLevel() {
		fmt.Fprintf(tw, "  %s\t%s\n", c.name, c.summary)
	}
	for _, g := range groups() {
		fmt.Fprintf(tw, "  %s <subcommand>\t%s\n", g.name, g.summary)
	}
	tw.Flush()

	fmt.Fprint(w, `
Shared flags:
  --socket <path>   Daemon socket (default $CLAUDE_SCHEDULER_SOCKET, then the config, then
                    `+config.DefaultSocket+`)
  --config <path>   Config file to read the socket from (default `+DefaultConfigPath+`)
  --json            Emit the daemon's JSON instead of a table

Run 'claude-scheduler <group>' to list a group's subcommands.
`)
}

// groupUsage lists one group's subcommands.
func groupUsage(w io.Writer, g group) {
	fmt.Fprintf(w, "%s: %s\n\n", g.name, g.summary)

	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	for _, c := range g.commands {
		fmt.Fprintf(tw, "  claude-scheduler %s\t%s\n", c.usage, c.summary)
	}
	tw.Flush()
}

// uptime renders a span in seconds. It deliberately does not go through
// relative(), which is for instants and would append "ago" to a duration.
func uptime(seconds float64) string {
	if seconds <= 0 {
		return "-"
	}
	d := time.Duration(seconds * float64(time.Second))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours()/24), int(d.Hours())%24)
	}
}
