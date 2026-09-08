package cli

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/raulsh/claude-scheduler/internal/store"
)

func kvCommands() []command {
	return []command{
		{
			name:    "set",
			usage:   "kv set <key> [<value>] [--file <path>] [--ttl <duration>]",
			summary: "store a value from an argument, a file, or stdin",
			build:   buildKVSet,
		},
		{
			name:    "get",
			usage:   "kv get <key>",
			summary: "write a value to stdout, byte for byte",
			build:   simple(runKVGet),
		},
		{
			name:    "list",
			usage:   "kv list [--prefix <p>]",
			summary: "list keys with their sizes",
			build:   buildKVList,
		},
		{
			name:    "del",
			usage:   "kv del <key>",
			summary: "remove a value",
			build:   simple(runKVDelete),
		},
	}
}

func buildKVSet() (func(*flag.FlagSet), func(context.Context, *env, []string) error) {
	var (
		file string
		ttl  string
		trim bool
	)
	return func(fs *flag.FlagSet) {
			fs.StringVar(&file, "file", "", "read the value from this file, or - for stdin")
			fs.StringVar(&ttl, "ttl", "", "expire the value after this long, e.g. 30m, 24h, 7d")
			fs.BoolVar(&trim, "trim", false, "strip trailing whitespace from the value")
		},
		func(ctx context.Context, e *env, args []string) error {
			if len(args) == 0 {
				return usageErrorf("kv set needs a key")
			}
			key, rest := args[0], args[1:]

			value, err := resolveValue(rest, file, stdinIsTerminal(), os.Stdin)
			if err != nil {
				return err
			}
			if trim {
				value = bytes.TrimRight(value, " \t\r\n")
			}

			return kvSet(ctx, e, key, value, ttl)
		}
}

// resolveValue applies the precedence rules for where a value comes from.
//
// Kept pure, and separate from the command, so the whole decision table can
// be tested without a process, a terminal or a daemon. Two rules generate
// it: an explicit source beats an implicit one, and two explicit sources
// are an error rather than a silent winner. That second rule is what stops
// `kv set k v < somefile` from quietly storing the file.
func resolveValue(positional []string, file string, stdinTTY bool, stdin io.Reader) ([]byte, error) {
	switch {
	case len(positional) > 1:
		return nil, usageErrorf("kv set takes one value; quote it if it contains spaces")

	case len(positional) == 1 && file != "":
		return nil, usageErrorf("give the value either as an argument or with --file, not both")

	case len(positional) == 1:
		return []byte(positional[0]), nil

	case file == "-":
		if stdinTTY {
			return nil, usageErrorf("--file - was given but stdin is a terminal")
		}
		return io.ReadAll(stdin)

	case file != "":
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}
		return data, nil

	case !stdinTTY:
		// Nothing explicit, but something is piped in.
		return io.ReadAll(stdin)

	default:
		return nil, usageErrorf("no value given: pass it as an argument, with --file, or on stdin")
	}
}

// stdinIsTerminal reports whether stdin is interactive, which is how
// `kv set k` with nothing piped is told from `echo v | kv set k`.
//
// A character device is the portable stdlib signal. go-isatty is in the
// module graph already, but only as an indirect dependency of the SQLite
// driver, and promoting it to a direct one for a single bit is not worth it.
func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		// Unknown: assume a terminal, so the failure is a clear error rather
		// than a read that blocks forever with nothing on screen.
		return true
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func kvSet(ctx context.Context, e *env, key string, value []byte, ttl string) error {
	path := "/api/v1/kv/" + escapeKey(key)
	if ttl != "" {
		path += "?ttl=" + url.QueryEscape(ttl)
	}

	raw, err := e.client.do(ctx, http.MethodPut, path,
		bytes.NewReader(value), "application/octet-stream")
	if err != nil {
		return err
	}
	if e.json {
		return printJSON(e.out, raw)
	}

	// Report the expiry, since a TTL is the one thing a caller cannot see
	// from the command they typed.
	var entry store.KVEntry
	if err := decode(raw, &entry); err == nil && entry.Expires() {
		fmt.Fprintf(e.out, "%s: %s, expires %s\n",
			key, bytesize(entry.Size), entry.ExpiresAt.Local().Format(time.RFC3339))
	}
	return nil
}

func runKVGet(ctx context.Context, e *env, args []string) error {
	if len(args) != 1 {
		return usageErrorf("kv get needs exactly one key")
	}

	raw, err := e.client.do(ctx, http.MethodGet, "/api/v1/kv/"+escapeKey(args[0]), nil, "")
	if err != nil {
		return err
	}
	// Written verbatim, with no trailing newline added: a file put in comes
	// back out byte-identical, and $(claude-scheduler kv get k) still works
	// because command substitution strips trailing newlines anyway.
	_, err = e.out.Write(raw)
	return err
}

func buildKVList() (func(*flag.FlagSet), func(context.Context, *env, []string) error) {
	var prefix string
	return func(fs *flag.FlagSet) {
			fs.StringVar(&prefix, "prefix", "", "only list keys starting with this")
		},
		func(ctx context.Context, e *env, args []string) error {
			if len(args) > 0 {
				return usageErrorf("kv list takes no arguments; use --prefix to filter")
			}

			path := "/api/v1/kv"
			if prefix != "" {
				path += "?prefix=" + url.QueryEscape(prefix)
			}

			raw, err := e.client.do(ctx, http.MethodGet, path, nil, "")
			if err != nil {
				return err
			}
			if e.json {
				return printJSON(e.out, raw)
			}

			var body struct {
				Entries []store.KVEntry `json:"entries"`
			}
			if err := decode(raw, &body); err != nil {
				return err
			}
			if len(body.Entries) == 0 {
				fmt.Fprintln(e.out, "no keys stored")
				return nil
			}

			rows := make([][]string, 0, len(body.Entries))
			for _, entry := range body.Entries {
				expires := "-"
				if entry.Expires() {
					expires = relative(entry.ExpiresAt)
				}
				rows = append(rows, []string{
					entry.Key,
					bytesize(entry.Size),
					relative(entry.UpdatedAt),
					expires,
				})
			}
			table(e.out, []string{"KEY", "SIZE", "UPDATED", "EXPIRES"}, rows)
			return nil
		}
}

func runKVDelete(ctx context.Context, e *env, args []string) error {
	if len(args) != 1 {
		return usageErrorf("kv del needs exactly one key")
	}
	_, err := e.client.do(ctx, http.MethodDelete, "/api/v1/kv/"+escapeKey(args[0]), nil, "")
	return err
}

// escapeKey renders a key into a URL path.
//
// Each segment is escaped separately so slashes inside a key stay slashes in
// the path, which is what the {key...} wildcard expects, while anything else
// that would change the path's meaning is encoded.
func escapeKey(key string) string {
	segments := strings.Split(key, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}
