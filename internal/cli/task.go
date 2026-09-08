package cli

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/raulsh/claude-scheduler/internal/store"
)

func taskCommands() []command {
	return []command{
		{name: "list", usage: "task list", summary: "list every task", build: simple(runTaskList)},
		{name: "show", usage: "task show <id|name>", summary: "show one task in full", build: simple(runTaskShow)},
		{name: "run", usage: "task run <id|name> [--follow]", summary: "trigger a run now", build: buildTaskRun},
		{name: "pause", usage: "task pause <id|name> [--reason <r>]", summary: "stop a task firing", build: buildTaskPause},
		{name: "resume", usage: "task resume <id|name>", summary: "let a paused task fire again", build: simple(runTaskResume)},
		{name: "preflight", usage: "task preflight <id|name>", summary: "check a task's dependencies without running it", build: simple(runTaskPreflight)},
		{name: "delete", usage: "task delete <id|name>", summary: "remove a task and its history", build: simple(runTaskDelete)},
	}
}

func runTaskList(ctx context.Context, e *env, args []string) error {
	if len(args) > 0 {
		return usageErrorf("task list takes no arguments")
	}

	raw, err := e.client.do(ctx, http.MethodGet, "/api/v1/tasks", nil, "")
	if err != nil {
		return err
	}
	if e.json {
		return printJSON(e.out, raw)
	}

	var body struct {
		Tasks []store.Task `json:"tasks"`
	}
	if err := decode(raw, &body); err != nil {
		return err
	}
	if len(body.Tasks) == 0 {
		fmt.Fprintln(e.out, "no tasks defined")
		return nil
	}

	rows := make([][]string, 0, len(body.Tasks))
	for _, t := range body.Tasks {
		next := "-"
		// NextRunAt is nil for a disabled or paused task, so it is never
		// dereferenced blindly.
		if t.NextRunAt != nil {
			next = relative(*t.NextRunAt)
		}
		rows = append(rows, []string{
			strconv.FormatInt(t.ID, 10),
			t.Name,
			t.CronExpr,
			taskState(t),
			next,
		})
	}
	table(e.out, []string{"ID", "NAME", "SCHEDULE", "STATE", "NEXT"}, rows)
	return nil
}

func taskState(t store.Task) string {
	switch {
	case !t.Enabled:
		return "disabled"
	case t.Paused:
		if t.PausedReason != "" {
			return "paused (" + t.PausedReason + ")"
		}
		return "paused"
	default:
		return "enabled"
	}
}

func runTaskShow(ctx context.Context, e *env, args []string) error {
	id, err := oneTask(ctx, e, args, "task show")
	if err != nil {
		return err
	}

	raw, err := e.client.do(ctx, http.MethodGet, taskPath(id, ""), nil, "")
	if err != nil {
		return err
	}
	if e.json {
		return printJSON(e.out, raw)
	}

	var t store.Task
	if err := decode(raw, &t); err != nil {
		return err
	}

	// Timeout carries json:"-", so only TimeoutSeconds survives a decode.
	fields := [][2]string{
		{"id", strconv.FormatInt(t.ID, 10)},
		{"name", t.Name},
		{"description", dash(t.Description)},
		{"schedule", t.CronExpr + " (" + t.Timezone + ")"},
		{"state", taskState(t)},
		{"model", dash(t.Model)},
		{"cwd", dash(t.Cwd)},
		{"timeout", duration(t.TimeoutSeconds * 1000)},
		{"budget", fmt.Sprintf("$%.2f", t.MaxBudgetUSD)},
		{"tools", dash(strings.Join(t.Tools, ", "))},
		{"allowed", dash(strings.Join(t.AllowedTools, ", "))},
		{"bypass", strconv.FormatBool(t.BypassPermissions)},
		{"overlap", t.OverlapPolicy},
		{"gating", t.GatingPolicy},
	}
	if t.NextRunAt != nil {
		fields = append(fields, [2]string{"next run", relative(*t.NextRunAt)})
	}

	rows := make([][]string, 0, len(fields))
	for _, f := range fields {
		rows = append(rows, []string{f[0], f[1]})
	}
	table(e.out, []string{"FIELD", "VALUE"}, rows)

	if len(t.Requirements) > 0 {
		fmt.Fprintln(e.out)
		reqs := make([][]string, 0, len(t.Requirements))
		for _, r := range t.Requirements {
			reqs = append(reqs, []string{r.Kind, r.Target, strconv.FormatBool(r.Required)})
		}
		table(e.out, []string{"REQUIRES", "TARGET", "REQUIRED"}, reqs)
	}

	fmt.Fprintf(e.out, "\nprompt:\n%s\n", t.Prompt)
	return nil
}

func buildTaskRun() (func(*flag.FlagSet), func(context.Context, *env, []string) error) {
	var follow bool
	return func(fs *flag.FlagSet) {
			fs.BoolVar(&follow, "follow", false, "stream the transcript until the run ends")
		},
		func(ctx context.Context, e *env, args []string) error {
			id, err := oneTask(ctx, e, args, "task run")
			if err != nil {
				return err
			}

			raw, err := e.client.do(ctx, http.MethodPost, taskPath(id, "run"), nil, "")
			if err != nil {
				return err
			}
			if e.json && !follow {
				return printJSON(e.out, raw)
			}

			var exec store.Execution
			if err := decode(raw, &exec); err != nil {
				return err
			}
			fmt.Fprintf(e.out, "execution %d %s\n", exec.ID, exec.Status)

			if !follow {
				return nil
			}
			return followExecution(ctx, e, exec.ID)
		}
}

func buildTaskPause() (func(*flag.FlagSet), func(context.Context, *env, []string) error) {
	var reason string
	return func(fs *flag.FlagSet) {
			fs.StringVar(&reason, "reason", "", "why the task is being paused")
		},
		func(ctx context.Context, e *env, args []string) error {
			id, err := oneTask(ctx, e, args, "task pause")
			if err != nil {
				return err
			}

			body := map[string]string{"reason": reason}
			raw, err := e.client.jsonBody(ctx, http.MethodPost, taskPath(id, "pause"), body)
			if err != nil {
				return err
			}
			return reportTask(e, raw, "paused")
		}
}

func runTaskResume(ctx context.Context, e *env, args []string) error {
	id, err := oneTask(ctx, e, args, "task resume")
	if err != nil {
		return err
	}
	raw, err := e.client.do(ctx, http.MethodPost, taskPath(id, "resume"), nil, "")
	if err != nil {
		return err
	}
	return reportTask(e, raw, "resumed")
}

func runTaskDelete(ctx context.Context, e *env, args []string) error {
	id, err := oneTask(ctx, e, args, "task delete")
	if err != nil {
		return err
	}
	if _, err := e.client.do(ctx, http.MethodDelete, taskPath(id, ""), nil, ""); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "task %d deleted\n", id)
	return nil
}

func runTaskPreflight(ctx context.Context, e *env, args []string) error {
	id, err := oneTask(ctx, e, args, "task preflight")
	if err != nil {
		return err
	}

	raw, err := e.client.do(ctx, http.MethodPost, taskPath(id, "preflight"), nil, "")
	if err != nil {
		return err
	}
	if e.json {
		return printJSON(e.out, raw)
	}

	var body struct {
		Results   []store.CheckResult `json:"results"`
		Failing   []store.CheckResult `json:"failing"`
		WouldRun  bool                `json:"would_run"`
		GatingPol string              `json:"gating_policy"`
	}
	if err := decode(raw, &body); err != nil {
		return err
	}

	printChecks(e, body.Results)
	fmt.Fprintf(e.out, "\nwould run: %t", body.WouldRun)
	if !body.WouldRun {
		fmt.Fprintf(e.out, " (%d failing, gating policy %s)", len(body.Failing), body.GatingPol)
	}
	fmt.Fprintln(e.out)
	return nil
}

func reportTask(e *env, raw []byte, verb string) error {
	if e.json {
		return printJSON(e.out, raw)
	}
	var t store.Task
	if err := decode(raw, &t); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "%s %s\n", t.Name, verb)
	return nil
}

func taskPath(id int64, action string) string {
	path := "/api/v1/tasks/" + strconv.FormatInt(id, 10)
	if action != "" {
		path += "/" + action
	}
	return path
}

// oneTask resolves a single task argument, which may be an id or a name.
//
// Accepting a name costs one extra request and saves looking an id up by
// hand every time; the API itself is id-only.
func oneTask(ctx context.Context, e *env, args []string, cmd string) (int64, error) {
	if len(args) != 1 {
		return 0, usageErrorf("%s needs exactly one task id or name", cmd)
	}
	return resolveTask(ctx, e, args[0])
}

func resolveTask(ctx context.Context, e *env, ref string) (int64, error) {
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		return id, nil
	}

	raw, err := e.client.do(ctx, http.MethodGet, "/api/v1/tasks", nil, "")
	if err != nil {
		return 0, err
	}
	var body struct {
		Tasks []store.Task `json:"tasks"`
	}
	if err := decode(raw, &body); err != nil {
		return 0, err
	}

	for _, t := range body.Tasks {
		if t.Name == ref {
			return t.ID, nil
		}
	}
	return 0, &apiError{status: http.StatusNotFound, msg: fmt.Sprintf("no task named %q", ref)}
}
