// Package claude wraps the claude CLI: locating the binary, building an
// argument list for a scheduled run, and decoding its stream-json output.
package claude

import (
	"encoding/json"
	"strings"
	"time"
)

// Event types emitted by `claude -p --output-format stream-json --verbose`.
const (
	TypeSystem    = "system"
	TypeAssistant = "assistant"
	TypeUser      = "user"
	TypeResult    = "result"
	TypeRateLimit = "rate_limit_event"
)

// MCP server statuses reported in a system/init event.
const (
	MCPConnected = "connected"
	MCPNeedsAuth = "needs-auth"
	MCPFailed    = "failed"
	MCPPending   = "pending"
)

// Event is one decoded line of the NDJSON stream. Raw is retained verbatim so
// the transcript can be stored and replayed without lossy re-encoding, and so
// fields this version does not model are not discarded.
type Event struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	UUID      string          `json:"uuid"`
	Timestamp time.Time       `json:"timestamp"`
	Raw       json.RawMessage `json:"-"`
}

// SystemInit is the payload of the system/init event. Its mcp_servers array
// is the only machine-readable view of MCP health the CLI offers, so it is
// recorded against every execution.
type SystemInit struct {
	CWD            string      `json:"cwd"`
	SessionID      string      `json:"session_id"`
	Tools          []string    `json:"tools"`
	MCPServers     []MCPServer `json:"mcp_servers"`
	Model          string      `json:"model"`
	PermissionMode string      `json:"permissionMode"`
	Version        string      `json:"claude_code_version"`
	APIKeySource   string      `json:"apiKeySource"`
}

// MCPServer is one entry of the system/init mcp_servers array.
type MCPServer struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Healthy reports whether the server is usable for a run.
func (m MCPServer) Healthy() bool { return m.Status == MCPConnected }

// AssistantEvent carries a model turn.
type AssistantEvent struct {
	Message struct {
		ID         string         `json:"id"`
		Model      string         `json:"model"`
		Role       string         `json:"role"`
		Content    []ContentBlock `json:"content"`
		StopReason string         `json:"stop_reason"`
		Usage      Usage          `json:"usage"`
	} `json:"message"`
	ParentToolUseID string `json:"parent_tool_use_id"`
}

// ContentBlock is one element of a message's content array. The union is
// discriminated by Type: text, thinking, tool_use or tool_result.
type ContentBlock struct {
	Type string `json:"type"`

	// type == "text"
	Text string `json:"text"`

	// type == "thinking"
	Thinking string `json:"thinking"`

	// type == "tool_use"
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// type == "tool_result"
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// UserEvent carries tool results fed back to the model.
type UserEvent struct {
	Message struct {
		Role    string         `json:"role"`
		Content []ContentBlock `json:"content"`
	} `json:"message"`
	ParentToolUseID string `json:"parent_tool_use_id"`
}

// Usage is the token accounting attached to turns and to the final result.
type Usage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
}

// Result is the terminal event of a run.
type Result struct {
	Subtype        string          `json:"subtype"`
	IsError        bool            `json:"is_error"`
	Result         string          `json:"result"`
	DurationMS     int64           `json:"duration_ms"`
	DurationAPIMS  int64           `json:"duration_api_ms"`
	NumTurns       int             `json:"num_turns"`
	TotalCostUSD   float64         `json:"total_cost_usd"`
	Usage          Usage           `json:"usage"`
	TerminalReason string          `json:"terminal_reason"`
	APIErrorStatus *string         `json:"api_error_status"`
	Denials        []Denial        `json:"permission_denials"`
	ModelUsage     json.RawMessage `json:"modelUsage"`
}

// Denial is one entry of permission_denials. The CLI does not document this
// shape, so every field is optional and DisplayRule degrades gracefully.
type Denial struct {
	ToolName  string          `json:"tool_name"`
	ToolUseID string          `json:"tool_use_id"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// DisplayRule renders a denial as an allowlist rule the user can approve.
// Bash denials become Bash(<first word> *) so re-running does not require
// allowing every possible command.
func (d Denial) DisplayRule() string {
	if d.ToolName == "" {
		return ""
	}
	if d.ToolName != "Bash" || len(d.ToolInput) == 0 {
		return d.ToolName
	}

	var input struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(d.ToolInput, &input); err != nil || input.Command == "" {
		return d.ToolName
	}
	if head, _, _ := strings.Cut(strings.TrimSpace(input.Command), " "); head != "" {
		return "Bash(" + head + " *)"
	}
	return d.ToolName
}

// RateLimit is the payload of a rate_limit_event.
type RateLimit struct {
	Info struct {
		Status                string          `json:"status"`
		ResetsAt              int64           `json:"resetsAt"`
		RateLimitType         string          `json:"rateLimitType"`
		OverageStatus         string          `json:"overageStatus"`
		OverageDisabledReason string          `json:"overageDisabledReason"`
		IsUsingOverage        bool            `json:"isUsingOverage"`
		UnifiedWindows        json.RawMessage `json:"unifiedWindows"`
	} `json:"rate_limit_info"`
}

// Exhausted reports whether this event means the run cannot proceed on
// credits or quota, as opposed to a routine allowed-usage notice. This is a
// distinct outcome from a task failure and must not be reported as one.
func (r RateLimit) Exhausted() bool {
	if r.Info.Status != "" && r.Info.Status != "allowed" {
		return true
	}
	// An allowed request with overage rejected still means the account has no
	// headroom left once the current window closes.
	return r.Info.OverageStatus == "rejected" && r.Info.OverageDisabledReason == "out_of_credits"
}

// ResetTime renders resetsAt as a time, zero when absent.
func (r RateLimit) ResetTime() time.Time {
	if r.Info.ResetsAt == 0 {
		return time.Time{}
	}
	return time.Unix(r.Info.ResetsAt, 0)
}

// DecodeSystemInit extracts the system/init payload.
func (e Event) DecodeSystemInit() (SystemInit, error) {
	var si SystemInit
	err := json.Unmarshal(e.Raw, &si)
	return si, err
}

// DecodeAssistant extracts an assistant turn.
func (e Event) DecodeAssistant() (AssistantEvent, error) {
	var a AssistantEvent
	err := json.Unmarshal(e.Raw, &a)
	return a, err
}

// DecodeUser extracts a tool-result turn.
func (e Event) DecodeUser() (UserEvent, error) {
	var u UserEvent
	err := json.Unmarshal(e.Raw, &u)
	return u, err
}

// DecodeResult extracts the terminal result event.
func (e Event) DecodeResult() (Result, error) {
	var r Result
	err := json.Unmarshal(e.Raw, &r)
	return r, err
}

// DecodeRateLimit extracts a rate limit notice.
func (e Event) DecodeRateLimit() (RateLimit, error) {
	var rl RateLimit
	err := json.Unmarshal(e.Raw, &rl)
	return rl, err
}

// IsSystemInit reports whether this event is the session preamble.
func (e Event) IsSystemInit() bool { return e.Type == TypeSystem && e.Subtype == "init" }
