package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/raulsh/claude-scheduler/internal/store"
)

func execCommands() []command {
	return []command{
		{name: "list", usage: "exec list [--task <id|name>] [--status <s>] [--limit <n>]", summary: "list recent executions", build: buildExecList},
		{name: "show", usage: "exec show <id>", summary: "show one execution in full", build: simple(runExecShow)},
		{name: "logs", usage: "exec logs <id> [--follow]", summary: "print an execution's transcript", build: buildExecLogs},
		{name: "cancel", usage: "exec cancel <id>", summary: "stop a running execution", build: simple(runExecCancel)},
	}
}

func buildExecList() (func(*flag.FlagSet), func(context.Context, *env, []string) error) {
	var (
		task   string
		status string
		limit  int
	)
	return func(fs *flag.FlagSet) {
			fs.StringVar(&task, "task", "", "only this task's executions, by id or name")
			fs.StringVar(&status, "status", "", "only these statuses, comma separated")
			fs.IntVar(&limit, "limit", 20, "how many executions to list")
		},
		func(ctx context.Context, e *env, args []string) error {
			if len(args) > 0 {
				return usageErrorf("exec list takes no arguments; use the flags to filter")
			}

			query := url.Values{}
			query.Set("limit", strconv.Itoa(limit))
			if task != "" {
				id, err := resolveTask(ctx, e, task)
				if err != nil {
					return err
				}
				query.Set("task_id", strconv.FormatInt(id, 10))
			}
			if status != "" {
				query.Set("status", status)
			}

			raw, err := e.client.do(ctx, http.MethodGet, "/api/v1/executions?"+query.Encode(), nil, "")
			if err != nil {
				return err
			}
			if e.json {
				return printJSON(e.out, raw)
			}

			var body struct {
				Executions []store.Execution `json:"executions"`
			}
			if err := decode(raw, &body); err != nil {
				return err
			}
			if len(body.Executions) == 0 {
				fmt.Fprintln(e.out, "no executions recorded")
				return nil
			}

			rows := make([][]string, 0, len(body.Executions))
			for _, x := range body.Executions {
				rows = append(rows, []string{
					strconv.FormatInt(x.ID, 10),
					dash(x.TaskName),
					x.Status,
					x.Trigger,
					relative(x.QueuedAt),
					duration(x.DurationMS),
					fmt.Sprintf("$%.4f", x.TotalCostUSD),
				})
			}
			table(e.out, []string{"ID", "TASK", "STATUS", "TRIGGER", "QUEUED", "TOOK", "COST"}, rows)
			return nil
		}
}

func runExecShow(ctx context.Context, e *env, args []string) error {
	id, err := oneID(args, "exec show")
	if err != nil {
		return err
	}

	raw, err := e.client.do(ctx, http.MethodGet, execPath(id, ""), nil, "")
	if err != nil {
		return err
	}
	if e.json {
		return printJSON(e.out, raw)
	}

	var x store.Execution
	if err := decode(raw, &x); err != nil {
		return err
	}

	fields := [][2]string{
		{"id", strconv.FormatInt(x.ID, 10)},
		{"task", dash(x.TaskName)},
		{"status", x.Status},
		{"trigger", x.Trigger},
		{"queued", relative(x.QueuedAt)},
		{"started", relative(x.StartedAt)},
		{"finished", relative(x.FinishedAt)},
		{"duration", duration(x.DurationMS)},
		{"cost", fmt.Sprintf("$%.4f", x.TotalCostUSD)},
		{"turns", strconv.Itoa(x.NumTurns)},
		{"model", dash(x.Model)},
		{"session", dash(x.ClaudeSessionID)},
	}
	// ExitCode and IsError are pointers: a run that never started has neither.
	if x.ExitCode != nil {
		fields = append(fields, [2]string{"exit code", strconv.Itoa(*x.ExitCode)})
	}
	if x.ErrorMessage != "" {
		fields = append(fields, [2]string{"error", x.ErrorMessage})
	}

	rows := make([][]string, 0, len(fields))
	for _, f := range fields {
		rows = append(rows, []string{f[0], f[1]})
	}
	table(e.out, []string{"FIELD", "VALUE"}, rows)

	if len(x.Checks) > 0 {
		fmt.Fprintln(e.out)
		printChecks(e, x.Checks)
	}
	if x.ResultText != "" {
		fmt.Fprintf(e.out, "\nresult:\n%s\n", x.ResultText)
	}
	return nil
}

func buildExecLogs() (func(*flag.FlagSet), func(context.Context, *env, []string) error) {
	var follow bool
	return func(fs *flag.FlagSet) {
			fs.BoolVar(&follow, "follow", false, "keep streaming until the run ends")
		},
		func(ctx context.Context, e *env, args []string) error {
			id, err := oneID(args, "exec logs")
			if err != nil {
				return err
			}
			// Following is a different endpoint, not a flag on this one: the
			// events endpoint returns a finite page, the stream endpoint
			// backfills and then stays open.
			if follow {
				return followExecution(ctx, e, id)
			}
			return printEvents(ctx, e, id)
		}
}

// printEvents renders the stored transcript of an execution.
func printEvents(ctx context.Context, e *env, id int64) error {
	raw, err := e.client.do(ctx, http.MethodGet, execPath(id, "events")+"?limit=10000", nil, "")
	if err != nil {
		return err
	}
	if e.json {
		return printJSON(e.out, raw)
	}

	var body struct {
		Events []struct {
			Seq     int64           `json:"seq"`
			Type    string          `json:"type"`
			Subtype string          `json:"subtype"`
			Payload json.RawMessage `json:"payload"`
		} `json:"events"`
	}
	if err := decode(raw, &body); err != nil {
		return err
	}

	r := newRenderer(e.out)
	for _, ev := range body.Events {
		r.render(ev.Type, ev.Subtype, ev.Payload)
	}
	return nil
}

func runExecCancel(ctx context.Context, e *env, args []string) error {
	id, err := oneID(args, "exec cancel")
	if err != nil {
		return err
	}
	if _, err := e.client.do(ctx, http.MethodPost, execPath(id, "cancel"), nil, ""); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "execution %d cancelled\n", id)
	return nil
}

func execPath(id int64, action string) string {
	path := "/api/v1/executions/" + strconv.FormatInt(id, 10)
	if action != "" {
		path += "/" + action
	}
	return path
}

func oneID(args []string, cmd string) (int64, error) {
	if len(args) != 1 {
		return 0, usageErrorf("%s needs exactly one execution id", cmd)
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return 0, usageErrorf("%q is not an execution id", args[0])
	}
	return id, nil
}
