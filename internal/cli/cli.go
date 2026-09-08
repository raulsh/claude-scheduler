// Package cli is the client side of claude-scheduler: everything that talks
// to a running daemon rather than being one.
//
// Every command here reaches the daemon over its Unix socket using the same
// HTTP API the web UI consumes. Nothing in this package opens the database
// directly: going through the daemon means a CLI write reloads the cron
// registrations and streams live events exactly as a UI write does.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/raulsh/claude-scheduler/internal/ipc"
)

// Exit codes. These are part of the interface: a task's prompt will script
// against this CLI, and "not found" has to be distinguishable from "the
// daemon is down" without parsing stderr.
const (
	ExitOK          = 0
	ExitError       = 1
	ExitUsage       = 2
	ExitUnreachable = 3
	ExitNotFound    = 4
)

// DefaultConfigPath matches the packaged location. The CLI reads the config
// only to discover the socket, and only if the file happens to be readable.
const DefaultConfigPath = "/etc/claude-scheduler/config.yaml"

// env is what every command needs: a client, and how to render output.
type env struct {
	client *client
	socket string
	json   bool
	out    io.Writer
}

// command is one leaf subcommand.
//
// build is called once per invocation and returns two closures over the same
// local variables: one to register the command's flags, one to run it. That
// keeps a command's flags next to the code that reads them, and typed,
// without a parse step that has to guess what each command accepts.
type command struct {
	name    string
	usage   string
	summary string
	build   func() (register func(*flag.FlagSet), run func(context.Context, *env, []string) error)
}

// simple wraps a command that takes no flags of its own.
func simple(run func(context.Context, *env, []string) error) func() (func(*flag.FlagSet), func(context.Context, *env, []string) error) {
	return func() (func(*flag.FlagSet), func(context.Context, *env, []string) error) {
		return func(*flag.FlagSet) {}, run
	}
}

type group struct {
	name     string
	summary  string
	commands []command
}

func groups() []group {
	return []group{
		{name: "task", summary: "inspect and manage scheduled tasks", commands: taskCommands()},
		{name: "exec", summary: "inspect executions and follow transcripts", commands: execCommands()},
		{name: "kv", summary: "read and write the key/value store", commands: kvCommands()},
	}
}

func topLevel() []command {
	return []command{
		{
			name: "status", usage: "status",
			summary: "report whether the daemon is serving",
			build:   simple(runStatus),
		},
		{
			name: "health", usage: "health [--kind <k>] [--force]",
			summary: "check the dependencies tasks declare",
			build:   buildHealth,
		},
	}
}

// Run dispatches a client command and returns a process exit code.
func Run(args []string) int {
	// Signals cancel the in-flight request rather than killing the process
	// mid-write, which matters while a transcript is streaming.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code, err := dispatch(ctx, args)
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "error: "+strings.TrimSuffix(err.Error(), "\n"))
	}
	return code
}

func dispatch(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		Usage(os.Stderr)
		return ExitUsage, nil
	}

	name, rest := args[0], args[1:]

	if cmd, ok := find(topLevel(), name); ok {
		return runLeaf(ctx, name, cmd, rest)
	}

	for _, g := range groups() {
		if g.name != name {
			continue
		}
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			groupUsage(os.Stderr, g)
			return ExitUsage, nil
		}
		cmd, ok := find(g.commands, rest[0])
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown %s command %q\n\n", g.name, rest[0])
			groupUsage(os.Stderr, g)
			return ExitUsage, nil
		}
		return runLeaf(ctx, g.name+" "+cmd.name, cmd, rest[1:])
	}

	fmt.Fprintf(os.Stderr, "unknown command %q\n\n", name)
	Usage(os.Stderr)
	return ExitUsage, nil
}

func find(cmds []command, name string) (command, bool) {
	for _, c := range cmds {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

// runLeaf resolves the shared flags, checks the daemon is up, and runs the
// command.
func runLeaf(ctx context.Context, path string, cmd command, args []string) (int, error) {
	fs := flag.NewFlagSet(path, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: claude-scheduler %s\n\n%s\n\nFlags:\n", cmd.usage, cmd.summary)
		fs.PrintDefaults()
	}

	socket := fs.String("socket", "", "path to the daemon socket")
	configPath := fs.String("config", DefaultConfigPath, "config file to read the socket from")
	asJSON := fs.Bool("json", false, "emit JSON instead of a table")

	// The command registers its own flags before parsing, so the leaf
	// decides what it accepts while the shared flags stay uniform.
	register, run := cmd.build()
	register(fs)

	positional, err := parseArgs(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK, nil
		}
		return ExitUsage, err
	}

	e := &env{
		socket: resolveSocket(*socket, *configPath),
		json:   *asJSON,
		out:    os.Stdout,
	}
	e.client = newClient(e.socket)

	// Every client command needs a daemon. Checking once, here, means a
	// stopped service produces one actionable message rather than a
	// transport error from somewhere deeper in the stack.
	if !ipc.Serving(e.socket) {
		return ExitUnreachable, ipc.NotServingError(e.socket)
	}

	return exitFor(run(ctx, e, positional))
}

// parseArgs lets a leaf command register its flags, then parses them out of
// args regardless of position.
//
// Go's flag package stops parsing at the first positional argument, which
// would make `kv set mykey value --ttl 1h` silently ignore the TTL. Sorting
// the flags ahead of the positionals first avoids a footgun that would only
// ever show up as a value that quietly never expired.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var flagArgs, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]

		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}

		flagArgs = append(flagArgs, arg)

		name, _, hasValue := splitFlag(arg)
		if hasValue {
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			// Unknown: let flag.Parse produce the error and the usage text.
			continue
		}
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			continue
		}
		// A value flag consumes the next argument.
		if i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}

	if err := fs.Parse(append(flagArgs, append([]string{"--"}, positional...)...)); err != nil {
		return nil, err
	}
	return fs.Args(), nil
}

// splitFlag breaks "--name=value" into its parts.
func splitFlag(arg string) (name, value string, hasValue bool) {
	trimmed := strings.TrimLeft(arg, "-")
	if before, after, found := strings.Cut(trimmed, "="); found {
		return before, after, true
	}
	return trimmed, "", false
}

// exitFor maps an error onto an exit code.
func exitFor(err error) (int, error) {
	if err == nil {
		return ExitOK, nil
	}

	var apiErr *apiError
	if errors.As(err, &apiErr) {
		switch apiErr.status {
		case http.StatusNotFound:
			return ExitNotFound, err
		case http.StatusServiceUnavailable:
			return ExitUnreachable, err
		}
		return ExitError, err
	}
	switch {
	case errors.Is(err, errUsage):
		return ExitUsage, err
	case errors.Is(err, context.Canceled):
		// Interrupting a follow is not a failure.
		return ExitOK, nil
	}
	return ExitError, err
}

// errUsage marks an error caused by how a command was invoked, so the
// dispatcher can map it onto the usage exit code.
var errUsage = errors.New("usage")

// usageError carries its own message rather than wrapping errUsage's text,
// which would print as "error: usage: ..." and read as two prefixes.
type usageError struct{ msg string }

func (e *usageError) Error() string        { return e.msg }
func (e *usageError) Is(target error) bool { return target == errUsage }

func usageErrorf(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}
