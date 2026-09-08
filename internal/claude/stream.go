package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// maxLineBytes caps a single NDJSON line. The system/init event alone is
// around 8 KB and grows with the tool and MCP inventory, so the limit is set
// well above that while still bounding memory on a pathological run.
const maxLineBytes = 8 << 20 // 8 MiB

// ErrLineTooLong reports an NDJSON line beyond maxLineBytes.
var ErrLineTooLong = errors.New("stream line exceeded the maximum size")

// Decoder reads the CLI's NDJSON output.
//
// A bufio.Scanner is deliberately not used: its default 64 KB token limit is
// already close to the size of a real system/init event. A json.Decoder is
// also avoided, because a single non-JSON line (a warning, say) would leave
// it unable to resynchronise. Reading whole lines and decoding each one
// independently tolerates both large events and unexpected noise.
type Decoder struct {
	r   *bufio.Reader
	seq int64
}

// NewDecoder wraps a reader of NDJSON.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: bufio.NewReaderSize(r, 64<<10)}
}

// Next returns the next event. Lines that are blank or not valid JSON are
// skipped and reported through skipped, so a stray diagnostic line does not
// abort an otherwise good run. io.EOF signals a clean end of stream.
func (d *Decoder) Next() (Event, []string, error) {
	var skipped []string

	for {
		line, err := d.readLine()
		if err != nil {
			return Event{}, skipped, err
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		// Only object lines are events; anything else is diagnostic output.
		if line[0] != '{' {
			skipped = append(skipped, string(line))
			continue
		}

		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			skipped = append(skipped, string(line))
			continue
		}

		// Retain the original bytes: the transcript is replayed from them,
		// and fields this version does not model must survive.
		ev.Raw = json.RawMessage(bytes.Clone(line))
		if ev.Timestamp.IsZero() {
			ev.Timestamp = time.Now()
		}
		d.seq++
		return ev, skipped, nil
	}
}

// Seq reports how many events have been returned so far.
func (d *Decoder) Seq() int64 { return d.seq }

// readLine reads one newline-terminated line of unbounded length, up to
// maxLineBytes.
func (d *Decoder) readLine() ([]byte, error) {
	var buf []byte
	for {
		chunk, err := d.r.ReadSlice('\n')
		if len(buf)+len(chunk) > maxLineBytes {
			return nil, fmt.Errorf("%w (%d bytes)", ErrLineTooLong, len(buf)+len(chunk))
		}

		switch {
		case err == nil:
			if buf == nil {
				return chunk, nil
			}
			return append(buf, chunk...), nil
		case errors.Is(err, bufio.ErrBufferFull):
			// The line is longer than the buffer; accumulate and continue.
			buf = append(buf, chunk...)
		case errors.Is(err, io.EOF):
			if len(buf)+len(chunk) == 0 {
				return nil, io.EOF
			}
			// A final line without a trailing newline is still an event.
			return append(buf, chunk...), nil
		default:
			return nil, err
		}
	}
}

// Summary accumulates the state of a run as its events arrive, so the
// executor does not have to re-walk the transcript when it finishes.
type Summary struct {
	SessionID      string
	Version        string
	Model          string
	MCPServers     []MCPServer
	PermissionMode string

	SawResult       bool
	Result          Result
	RateLimited     bool
	RateLimitDetail string

	AssistantTurns int
	ToolCalls      int
}

// Observe folds one event into the summary.
func (s *Summary) Observe(ev Event) {
	switch {
	case ev.IsSystemInit():
		if si, err := ev.DecodeSystemInit(); err == nil {
			s.SessionID = si.SessionID
			s.Version = si.Version
			s.Model = si.Model
			s.MCPServers = si.MCPServers
			s.PermissionMode = si.PermissionMode
		}

	case ev.Type == TypeAssistant:
		s.AssistantTurns++
		if a, err := ev.DecodeAssistant(); err == nil {
			for _, block := range a.Message.Content {
				if block.Type == "tool_use" {
					s.ToolCalls++
				}
			}
			if a.Message.Model != "" {
				s.Model = a.Message.Model
			}
		}

	case ev.Type == TypeRateLimit:
		if rl, err := ev.DecodeRateLimit(); err == nil && rl.Exhausted() {
			s.RateLimited = true
			s.RateLimitDetail = rateLimitDetail(rl)
		}

	case ev.Type == TypeResult:
		if r, err := ev.DecodeResult(); err == nil {
			s.SawResult = true
			s.Result = r
		}
	}

	if ev.SessionID != "" && s.SessionID == "" {
		s.SessionID = ev.SessionID
	}
}

func rateLimitDetail(rl RateLimit) string {
	detail := "rate limit reached"
	if rl.Info.RateLimitType != "" {
		detail += " (" + rl.Info.RateLimitType + ")"
	}
	if rl.Info.OverageDisabledReason == "out_of_credits" {
		detail = "out of credits; overage is disabled"
	}
	if t := rl.ResetTime(); !t.IsZero() {
		detail += ", resets " + t.Format(time.RFC3339)
	}
	return detail
}

// DenialRules renders the run's permission denials as allowlist rules,
// deduplicated, which is what the "allow these and re-run" action applies.
func (r Result) DenialRules() []string {
	seen := make(map[string]bool, len(r.Denials))
	var out []string
	for _, d := range r.Denials {
		rule := d.DisplayRule()
		if rule == "" || seen[rule] {
			continue
		}
		seen[rule] = true
		out = append(out, rule)
	}
	return out
}
