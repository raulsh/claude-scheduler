package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// table writes aligned columns.
//
// No rules and no box drawing: this output is meant to survive being piped
// into grep and awk, so it is columns of text and nothing else.
func table(w io.Writer, header []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

// printJSON relays the daemon's own response, indented.
//
// The bytes are the daemon's rather than a re-encoding of whatever fields
// this CLI happens to model, which makes --json exactly the HTTP API and
// keeps it correct as the API grows.
func printJSON(w io.Writer, raw []byte) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}

	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		// Not JSON after all: pass it through rather than swallow it.
		_, err := w.Write(raw)
		return err
	}
	buf.WriteByte('\n')
	_, err := buf.WriteTo(w)
	return err
}

// dash renders an empty value as a placeholder, so a column never looks
// like it is missing rather than empty.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// relative renders a time as an offset from now, which is what someone
// scanning a list of runs actually wants to know.
func relative(t time.Time) string {
	if t.IsZero() {
		return "-"
	}

	d := time.Since(t)
	future := d < 0
	if future {
		d = -d
	}

	var out string
	switch {
	case d < time.Minute:
		out = fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		out = fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		out = fmt.Sprintf("%dh", int(d.Hours()))
	default:
		out = fmt.Sprintf("%dd", int(d.Hours()/24))
	}

	if future {
		return "in " + out
	}
	return out + " ago"
}

// duration renders a millisecond count compactly.
func duration(ms int64) string {
	if ms == 0 {
		return "-"
	}
	d := time.Duration(ms) * time.Millisecond
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return d.Round(time.Second).String()
}

// bytesize renders a value size for the kv listing.
func bytesize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fK", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/(1024*1024))
	}
}
