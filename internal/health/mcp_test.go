package health

import (
	"strings"
	"testing"
	"time"

	"github.com/raulsh/claude-scheduler/internal/store"
)

// realMCPListOutput is verbatim output from `claude mcp list` on this
// machine, covering all four status markers.
//
// Do not reformat this string. The glyphs and the em-dash before the failure
// reason are what the CLI actually prints, and the parser is written against
// them. "Tidying" the punctuation here would make the test stop describing
// reality while still passing.
const realMCPListOutput = `Checking MCP server health…

claude.ai Vambe: https://api.vambe.me/api/public/mcp - ! Needs authentication
claude.ai Google Cloud BigQuery: https://bigquery.googleapis.com/mcp - ! Needs authentication
claude.ai CaseTracking: https://mcp.thecasetracking.com/mcp - ✔ Connected
claude.ai Windsor.ai: https://mcp.windsor.ai - ! Needs authentication
claude.ai Google Drive: https://drivemcp.googleapis.com/mcp/v1 - ✔ Connected
lemn-signals: https://mcp.lemn.sh/signals (HTTP) - ✔ Connected
claude-design: https://api.anthropic.com/v1/design/mcp (HTTP) - ✘ Failed to connect — api.anthropic.com rejected your claude.ai login for Claude Design (HTTP 403), most likely because a /login token carries no Claude Design access. Run /design-login and retry.
project-server: /usr/local/bin/some-server --flag - ⏸ Pending approval
`

func TestParseMCPListRealOutput(t *testing.T) {
	got := parseMCPList(realMCPListOutput, 12*time.Second)

	if len(got) != 8 {
		t.Fatalf("parsed %d servers, want 8: %v", len(got), keysOf(got))
	}

	cases := map[string]State{
		"claude.ai Vambe":                 store.CheckNeedsLogin,
		"claude.ai Google Cloud BigQuery": store.CheckNeedsLogin,
		"claude.ai CaseTracking":          store.CheckOK,
		"claude.ai Windsor.ai":            store.CheckNeedsLogin,
		"claude.ai Google Drive":          store.CheckOK,
		"lemn-signals":                    store.CheckOK,
		"claude-design":                   store.CheckUnavailable,
		"project-server":                  store.CheckMisconfigured,
	}
	for name, want := range cases {
		res, found := got[name]
		if !found {
			t.Errorf("server %q was not parsed", name)
			continue
		}
		if res.State != want {
			t.Errorf("%q state = %q, want %q (detail %q)", name, res.State, want, res.Detail)
		}
	}

	// A name containing a dot must not be truncated.
	if _, ok := got["claude.ai Windsor.ai"]; !ok {
		t.Error("a server name with dots was mis-parsed")
	}

	// The long em-dash error text must survive intact for the UI.
	design := got["claude-design"]
	if !strings.Contains(design.Detail, "HTTP 403") {
		t.Errorf("failure detail was lost: %q", design.Detail)
	}

	// A failed connection is transient by design: one flaky server must not
	// be able to pause every schedule that references it.
	if design.Gates() {
		t.Error("a failed MCP connection must not gate an execution")
	}
	// Needing authentication does gate, and offers remediation.
	vambe := got["claude.ai Vambe"]
	if !vambe.Gates() {
		t.Error("a server needing authentication must gate")
	}
	if vambe.Remediation == nil {
		t.Fatal("no remediation offered for a server needing login")
	}
	if vambe.Remediation.Automatic {
		t.Error("MCP login needs a redirect URL pasted back, so it is not automatic")
	}
	if !strings.Contains(vambe.Remediation.Command, "claude mcp login") {
		t.Errorf("remediation command = %q", vambe.Remediation.Command)
	}

	// Pending approval gates: the task cannot work until it is accepted.
	if !got["project-server"].Gates() {
		t.Error("a pending-approval server must gate")
	}
}

// TestParseMCPListStdioServer covers a server shown as a command rather than
// a URL, where the detail field contains its own spaces and flags.
func TestParseMCPListStdioServer(t *testing.T) {
	out := "my-server: npx -y @scope/mcp-server --port 3000 - ✔ Connected\n"
	got := parseMCPList(out, 0)

	res, ok := got["my-server"]
	if !ok {
		t.Fatalf("stdio server not parsed: %v", keysOf(got))
	}
	if res.State != store.CheckOK {
		t.Errorf("state = %q, want ok", res.State)
	}
	if !strings.Contains(res.Detail, "npx -y @scope/mcp-server") {
		t.Errorf("command detail lost: %q", res.Detail)
	}
}

func TestParseMCPListIgnoresNoise(t *testing.T) {
	out := `Checking MCP server health…

Some unexpected banner line without the expected shape
good: https://x - ✔ Connected
`
	got := parseMCPList(out, 0)
	if len(got) != 1 {
		t.Errorf("parsed %d servers, want 1: %v", len(got), keysOf(got))
	}
}

func TestParseMCPListEmpty(t *testing.T) {
	if got := parseMCPList("Checking MCP server health…\n\n", 0); len(got) != 0 {
		t.Errorf("parsed %d servers from empty output", len(got))
	}
}

func TestMCPMissingBinary(t *testing.T) {
	c := &MCPChecker{Binary: ""}
	if _, err := c.CheckAll(nil); err == nil {
		t.Error("CheckAll should fail when the claude CLI is missing")
	}
}

func keysOf(m map[string]Result) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
