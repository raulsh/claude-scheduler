package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveValue walks the whole decision table for where `kv set` takes
// its value from. The table is the feature, so it is what gets tested.
func TestResolveValue(t *testing.T) {
	file := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(file, []byte(`{"from":"file"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		positional []string
		file       string
		stdinTTY   bool
		stdin      string
		want       string
		wantErr    bool
	}{
		{
			name: "argument wins", positional: []string{"hello"},
			stdinTTY: true, want: "hello",
		},
		{
			// The important one: a redirect must not quietly beat the
			// argument that was actually typed.
			name: "argument beats a redirect", positional: []string{"hello"},
			stdinTTY: false, stdin: "piped", want: "hello",
		},
		{
			name: "file", file: file, stdinTTY: true, want: `{"from":"file"}`,
		},
		{
			name: "explicit stdin", file: "-",
			stdinTTY: false, stdin: "from stdin", want: "from stdin",
		},
		{
			name: "implicit stdin", stdinTTY: false,
			stdin: `{"cursor":42}`, want: `{"cursor":42}`,
		},
		{
			name: "empty stdin is a value", stdinTTY: false, stdin: "", want: "",
		},
		{
			name: "argument and file together", positional: []string{"hello"},
			file: file, stdinTTY: true, wantErr: true,
		},
		{
			name: "explicit stdin at a terminal", file: "-",
			stdinTTY: true, wantErr: true,
		},
		{
			name: "nothing at a terminal", stdinTTY: true, wantErr: true,
		},
		{
			name: "two positionals", positional: []string{"a", "b"},
			stdinTTY: true, wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveValue(c.positional, c.file, c.stdinTTY, strings.NewReader(c.stdin))
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				// Every one of these is the caller's mistake, so they must
				// map onto the usage exit code rather than a generic failure.
				if !errors.Is(err, errUsage) {
					t.Errorf("error should be a usage error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveValue: %v", err)
			}
			if string(got) != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestResolveValueKeepsBytesExact guards the byte-for-byte contract: a
// trailing newline from `echo` is part of the value, because a store that
// silently trims what it was handed cannot answer "is this what I stored".
func TestResolveValueKeepsBytesExact(t *testing.T) {
	got, err := resolveValue(nil, "", false, strings.NewReader("x\n"))
	if err != nil {
		t.Fatalf("resolveValue: %v", err)
	}
	if string(got) != "x\n" {
		t.Errorf("got %q, want the trailing newline preserved", got)
	}
}

func TestEscapeKeyKeepsSlashes(t *testing.T) {
	cases := map[string]string{
		"simple":          "simple",
		"report/cursor":   "report/cursor",
		"a b":             "a%20b",
		"deep/a/b/c":      "deep/a/b/c",
		"question?mark":   "question%3Fmark",
		"percent%encoded": "percent%25encoded",
	}
	for key, want := range cases {
		if got := escapeKey(key); got != want {
			t.Errorf("escapeKey(%q) = %q, want %q", key, got, want)
		}
	}
}
