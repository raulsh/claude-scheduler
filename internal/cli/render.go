package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/raulsh/claude-scheduler/internal/claude"
)

// renderer turns transcript events into readable terminal output.
//
// It decodes through internal/claude rather than reimplementing the event
// shapes, so the CLI and the web UI derive their views of a run from the
// same code.
type renderer struct {
	out io.Writer
	// calls maps a tool_use id to its tool name, so a result arriving later
	// can be labelled with the call it answers.
	calls   map[string]string
	summary claude.Summary
}

func newRenderer(out io.Writer) *renderer {
	return &renderer{out: out, calls: map[string]string{}}
}

// render writes one event.
func (r *renderer) render(eventType, subtype string, payload json.RawMessage) {
	if len(payload) == 0 {
		return
	}

	// claude.Event.Raw is tagged json:"-", so it has to be assigned rather
	// than unmarshalled: decoding the payload into an Event would leave Raw
	// empty and every Decode method with nothing to read.
	ev := claude.Event{Type: eventType, Subtype: subtype, Raw: payload}
	r.summary.Observe(ev)

	switch eventType {
	case claude.TypeSystem:
		r.system(ev)
	case claude.TypeAssistant:
		r.assistant(ev)
	case claude.TypeUser:
		r.toolResults(ev)
	case claude.TypeRateLimit:
		r.rateLimit(ev)
	case claude.TypeResult:
		r.result(ev)
	}
}

func (r *renderer) system(ev claude.Event) {
	if !ev.IsSystemInit() {
		return
	}
	init, err := ev.DecodeSystemInit()
	if err != nil {
		return
	}

	healthy := 0
	for _, s := range init.MCPServers {
		if s.Healthy() {
			healthy++
		}
	}
	fmt.Fprintf(r.out, "session %s  cli %s  %d tools",
		dash(init.Model), dash(init.Version), len(init.Tools))
	if len(init.MCPServers) > 0 {
		fmt.Fprintf(r.out, "  mcp %d/%d", healthy, len(init.MCPServers))
	}
	fmt.Fprintln(r.out)
}

func (r *renderer) assistant(ev claude.Event) {
	msg, err := ev.DecodeAssistant()
	if err != nil {
		return
	}

	for _, block := range msg.Message.Content {
		switch block.Type {
		case "text":
			if text := strings.TrimSpace(block.Text); text != "" {
				fmt.Fprintln(r.out, text)
			}
		case "tool_use":
			r.calls[block.ID] = block.Name
			fmt.Fprintf(r.out, "  -> %s %s\n", block.Name, oneLine(block.Input, 120))
		}
	}
}

func (r *renderer) toolResults(ev claude.Event) {
	msg, err := ev.DecodeUser()
	if err != nil {
		return
	}

	for _, block := range msg.Message.Content {
		if block.Type != "tool_result" {
			continue
		}
		mark := "ok"
		if block.IsError {
			mark = "failed"
		}
		fmt.Fprintf(r.out, "  <- %s %s\n", dash(r.calls[block.ToolUseID]), mark)
	}
}

func (r *renderer) rateLimit(ev claude.Event) {
	rl, err := ev.DecodeRateLimit()
	// Only an exhausted limit is worth reporting; a routine usage notice is
	// not a problem.
	if err != nil || !rl.Exhausted() {
		return
	}
	fmt.Fprint(r.out, "rate limited")
	if reset := rl.ResetTime(); !reset.IsZero() {
		fmt.Fprintf(r.out, ", resets %s", relative(reset))
	}
	fmt.Fprintln(r.out)
}

func (r *renderer) result(ev claude.Event) {
	res, err := ev.DecodeResult()
	if err != nil {
		return
	}

	if text := strings.TrimSpace(res.Result); text != "" {
		fmt.Fprintf(r.out, "\n%s\n", text)
	}
	fmt.Fprintf(r.out, "\n%d turns  %s  $%.4f\n",
		res.NumTurns, duration(res.DurationMS), res.TotalCostUSD)

	// Denials are rendered as the exact allowlist rules that would let the
	// run through, which is what someone has to add to the task.
	var rules []string
	for _, d := range res.Denials {
		if rule := d.DisplayRule(); rule != "" {
			rules = append(rules, rule)
		}
	}
	if len(rules) > 0 {
		fmt.Fprintf(r.out, "denied: %s\n", strings.Join(rules, ", "))
	}
}

// oneLine compacts a JSON payload onto a single truncated line, so a tool
// call's arguments are visible without a wall of pretty-printed input.
func oneLine(raw json.RawMessage, limit int) string {
	if len(raw) == 0 {
		return ""
	}

	var buf strings.Builder
	for _, b := range raw {
		switch b {
		case '\n', '\t', '\r':
			buf.WriteByte(' ')
		default:
			buf.WriteByte(b)
		}
	}

	out := strings.Join(strings.Fields(buf.String()), " ")
	if len(out) > limit {
		return out[:limit] + "..."
	}
	return out
}
