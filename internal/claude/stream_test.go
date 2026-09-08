package claude

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// TestDecodeRealRun parses a stream captured from an actual claude CLI run.
func TestDecodeRealRun(t *testing.T) {
	f, err := os.Open("testdata/run-success.ndjson")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	dec := NewDecoder(f)
	var (
		events     []Event
		allSkipped []string
		summary    Summary
	)
	for {
		ev, skipped, err := dec.Next()
		allSkipped = append(allSkipped, skipped...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		events = append(events, ev)
		summary.Observe(ev)
	}

	if len(events) != 6 {
		t.Fatalf("decoded %d events, want 6", len(events))
	}
	// The non-JSON line must be reported, not fatal.
	if len(allSkipped) != 1 || !strings.Contains(allSkipped[0], "not json") {
		t.Errorf("skipped = %#v, want the one noise line", allSkipped)
	}

	if summary.SessionID != "31b4c857-001d-4107-bf3d-bb2f5fa8e36a" {
		t.Errorf("SessionID = %q", summary.SessionID)
	}
	if summary.Version != "2.1.263" {
		t.Errorf("Version = %q, want 2.1.263", summary.Version)
	}
	if summary.Model != "claude-sonnet-5" {
		t.Errorf("Model = %q", summary.Model)
	}
	if summary.AssistantTurns != 2 {
		t.Errorf("AssistantTurns = %d, want 2", summary.AssistantTurns)
	}
	if summary.ToolCalls != 1 {
		t.Errorf("ToolCalls = %d, want 1", summary.ToolCalls)
	}

	// The MCP snapshot is the whole point of reading system/init.
	if len(summary.MCPServers) != 5 {
		t.Fatalf("MCPServers = %d, want 5", len(summary.MCPServers))
	}
	healthy := 0
	for _, m := range summary.MCPServers {
		if m.Healthy() {
			healthy++
		}
	}
	if healthy != 2 {
		t.Errorf("healthy MCP servers = %d, want 2", healthy)
	}

	if !summary.SawResult {
		t.Fatal("no result event observed")
	}
	if summary.Result.TotalCostUSD == 0 {
		t.Error("TotalCostUSD not captured")
	}
	if summary.Result.IsError {
		t.Error("IsError should be false for a successful run")
	}
	if summary.Result.Subtype != "success" {
		t.Errorf("Subtype = %q", summary.Result.Subtype)
	}
	if summary.Result.Usage.CacheReadTokens != 19004 {
		t.Errorf("CacheReadTokens = %d", summary.Result.Usage.CacheReadTokens)
	}
}

// TestRateLimitExhaustedIsNotAFailure guards the distinction the UI depends
// on: an out-of-credits run is reported separately from a broken task.
func TestRateLimitExhausted(t *testing.T) {
	cases := []struct {
		name string
		json string
		want bool
	}{
		{
			name: "allowed with overage rejected and no credits",
			json: `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","overageStatus":"rejected","overageDisabledReason":"out_of_credits"}}`,
			want: true,
		},
		{
			name: "allowed with overage available",
			json: `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","overageStatus":"allowed"}}`,
			want: false,
		},
		{
			name: "explicitly rejected",
			json: `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}`,
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec := NewDecoder(strings.NewReader(tc.json + "\n"))
			ev, _, err := dec.Next()
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			rl, err := ev.DecodeRateLimit()
			if err != nil {
				t.Fatalf("DecodeRateLimit: %v", err)
			}
			if got := rl.Exhausted(); got != tc.want {
				t.Errorf("Exhausted() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLargeLine proves a line far beyond bufio.Scanner's 64 KB default token
// limit decodes correctly. A real system/init is already ~8 KB and grows with
// the tool and MCP inventory.
func TestLargeLine(t *testing.T) {
	tools := make([]string, 20000)
	for i := range tools {
		tools[i] = "mcp__some_long_server_name__some_long_tool_name"
	}
	payload, err := json.Marshal(map[string]any{
		"type":                "system",
		"subtype":             "init",
		"session_id":          "big",
		"tools":               tools,
		"claude_code_version": "2.1.263",
		"mcp_servers":         []MCPServer{{Name: "x", Status: "connected"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(payload) < 128<<10 {
		t.Fatalf("fixture is only %d bytes; it must exceed the Scanner default", len(payload))
	}

	dec := NewDecoder(strings.NewReader(string(payload) + "\n"))
	ev, _, err := dec.Next()
	if err != nil {
		t.Fatalf("Next on a %d byte line: %v", len(payload), err)
	}
	si, err := ev.DecodeSystemInit()
	if err != nil {
		t.Fatalf("DecodeSystemInit: %v", err)
	}
	if len(si.Tools) != 20000 {
		t.Errorf("Tools = %d, want 20000", len(si.Tools))
	}
	if si.Version != "2.1.263" {
		t.Errorf("Version = %q", si.Version)
	}
}

func TestFinalLineWithoutNewline(t *testing.T) {
	dec := NewDecoder(strings.NewReader(`{"type":"result","subtype":"success","is_error":false}`))
	ev, _, err := dec.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Type != TypeResult {
		t.Errorf("Type = %q", ev.Type)
	}
	if _, _, err := dec.Next(); err != io.EOF {
		t.Errorf("second Next err = %v, want io.EOF", err)
	}
}

func TestDenialRules(t *testing.T) {
	r := Result{Denials: []Denial{
		{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"rm -rf /tmp/x"}`)},
		{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"rm -rf /tmp/y"}`)},
		{ToolName: "Write"},
		{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"curl https://x"}`)},
	}}

	got := r.DenialRules()
	want := []string{"Bash(rm *)", "Write", "Bash(curl *)"}
	if len(got) != len(want) {
		t.Fatalf("DenialRules() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rule[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestEmptyStream(t *testing.T) {
	dec := NewDecoder(strings.NewReader(""))
	if _, _, err := dec.Next(); err != io.EOF {
		t.Errorf("err = %v, want io.EOF", err)
	}
}
