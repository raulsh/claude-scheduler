package claude

import (
	"slices"
	"strings"
	"testing"
)

// TestArgsAppendsSystemPrompt checks the flag is passed as its own argument
// pair. --append-system-prompt takes one value, so the text has to arrive
// whole rather than split on whitespace, and it must not displace the
// --allowedTools value, which is variadic and swallows what follows it.
func TestArgsAppendsSystemPrompt(t *testing.T) {
	args := CLI{Path: "claude"}.Args(RunOptions{
		AllowedTools:       []string{"Bash(claude-scheduler:*)"},
		AppendSystemPrompt: KVBasePrompt,
	})

	i := slices.Index(args, "--append-system-prompt")
	if i < 0 {
		t.Fatalf("Args = %v, want an --append-system-prompt flag", args)
	}
	if i == len(args)-1 {
		t.Fatal("--append-system-prompt is the last argument, so it carries no value")
	}
	if got := args[i+1]; got != KVBasePrompt {
		t.Errorf("appended prompt = %q, want the base prompt", got)
	}

	j := slices.Index(args, "--allowedTools")
	if j < 0 || args[j+1] != "Bash(claude-scheduler:*)" {
		t.Errorf("Args = %v, want --allowedTools to keep its own value", args)
	}
}

// TestArgsOmitsEmptySystemPrompt keeps a run with nothing to add identical
// to what it was before the flag existed: an empty --append-system-prompt
// would still count as a system prompt to the CLI.
func TestArgsOmitsEmptySystemPrompt(t *testing.T) {
	args := CLI{Path: "claude"}.Args(RunOptions{})
	if slices.Contains(args, "--append-system-prompt") {
		t.Errorf("Args = %v, want no --append-system-prompt", args)
	}
}

// TestKVBasePromptIsConditional guards the property the whole design rests
// on: the text tells a run to use the store only when its prompt needs
// state, so a task that needs none behaves as it always did.
func TestKVBasePromptIsConditional(t *testing.T) {
	for _, want := range []string{"only when", "ignore the store completely"} {
		if !strings.Contains(KVBasePrompt, want) {
			t.Errorf("base prompt no longer says %q; it now reads as an instruction to use the store", want)
		}
	}
}
