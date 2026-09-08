package cli

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/raulsh/claude-scheduler/internal/store"
)

// runStatus reports that the daemon is up and what it thinks of itself.
//
// Reaching this point already proves the socket is serving, since every
// command probes it first, so this is about the daemon's own view: version,
// database, and how many dependencies are currently gating runs.
func runStatus(ctx context.Context, e *env, args []string) error {
	if len(args) > 0 {
		return usageErrorf("status takes no arguments")
	}

	raw, err := e.client.do(ctx, http.MethodGet, "/api/v1/health", nil, "")
	if err != nil {
		return err
	}
	if e.json {
		return printJSON(e.out, raw)
	}

	var health struct {
		Status   string   `json:"status"`
		Version  string   `json:"version"`
		Uptime   float64  `json:"uptime_seconds"`
		Database string   `json:"database"`
		UIBuilt  bool     `json:"ui_built"`
		Warnings []string `json:"warnings"`
		Binaries struct {
			Claude string `json:"claude"`
			AWS    string `json:"aws"`
		} `json:"binaries"`
	}
	if err := decode(raw, &health); err != nil {
		return err
	}

	rows := [][]string{
		{"socket", e.socket},
		{"status", health.Status},
		{"version", health.Version},
		{"uptime", uptime(health.Uptime)},
		{"database", health.Database},
		{"claude", dash(health.Binaries.Claude)},
		{"aws", dash(health.Binaries.AWS)},
	}
	table(e.out, []string{"FIELD", "VALUE"}, rows)

	for _, w := range health.Warnings {
		fmt.Fprintf(e.out, "\nwarning: %s\n", w)
	}
	return nil
}

func buildHealth() (func(*flag.FlagSet), func(context.Context, *env, []string) error) {
	var (
		kind  string
		force bool
	)
	return func(fs *flag.FlagSet) {
			fs.StringVar(&kind, "kind", "", "only this kind of dependency: aws_profile, mcp_server, binary")
			fs.BoolVar(&force, "force", false, "re-run the checks instead of using cached results")
		},
		func(ctx context.Context, e *env, args []string) error {
			if len(args) > 0 {
				return usageErrorf("health takes no arguments; use --kind to filter")
			}

			query := url.Values{}
			if force {
				query.Set("force", "1")
			}

			// The kind endpoints are separate from the snapshot, and the
			// target segment is unescaped by the handler, so an MCP server
			// name containing a slash has to be escaped here.
			path := "/api/v1/health/snapshot"
			if kind != "" {
				path = "/api/v1/health/" + url.PathEscape(kind)
			}
			if len(query) > 0 {
				path += "?" + query.Encode()
			}

			raw, err := e.client.do(ctx, http.MethodGet, path, nil, "")
			if err != nil {
				return err
			}
			if e.json {
				return printJSON(e.out, raw)
			}

			if kind != "" {
				var body struct {
					Results []store.CheckResult `json:"results"`
				}
				if err := decode(raw, &body); err != nil {
					return err
				}
				printChecks(e, body.Results)
				return nil
			}

			var body struct {
				Kinds map[string][]store.CheckResult `json:"kinds"`
			}
			if err := decode(raw, &body); err != nil {
				return err
			}

			var all []store.CheckResult
			for _, results := range body.Kinds {
				all = append(all, results...)
			}
			printChecks(e, all)
			return nil
		}
}

// printChecks renders dependency check results.
func printChecks(e *env, results []store.CheckResult) {
	if len(results) == 0 {
		fmt.Fprintln(e.out, "no dependency checks recorded")
		return
	}

	rows := make([][]string, 0, len(results))
	for _, c := range results {
		// Gates is the distinction that matters: a dependency that is merely
		// unreachable does not stop a run, an expired credential does.
		gating := ""
		if store.Gates(c.State) {
			gating = "blocks runs"
		}
		rows = append(rows, []string{
			c.Kind,
			c.Target,
			c.State,
			gating,
			strconv.FormatInt(c.LatencyMS, 10) + "ms",
			truncate(c.Detail, 60),
		})
	}
	table(e.out, []string{"KIND", "TARGET", "STATE", "GATING", "LATENCY", "DETAIL"}, rows)
}

func truncate(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= limit {
		return dash(s)
	}
	return s[:limit] + "..."
}
