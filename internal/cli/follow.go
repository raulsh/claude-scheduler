package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/raulsh/claude-scheduler/internal/executor"
)

// errTerminal ends a follow: the run reached a final state.
var errTerminal = errors.New("execution finished")

// maxReconnects bounds how many times a dropped stream is retried before
// giving up, so a daemon that keeps closing the connection does not spin.
const maxReconnects = 5

// followExecution streams an execution's transcript until it ends.
//
// Reconnects resume rather than restart: the daemon replays from
// Last-Event-ID out of the database before attaching to the live feed, so a
// dropped connection loses no events and duplicates none.
func followExecution(ctx context.Context, e *env, id int64) error {
	r := newRenderer(e.out)
	lastSeq := int64(0)
	backoff := 250 * time.Millisecond

	for attempt := 0; ; attempt++ {
		header := http.Header{"Accept": {"text/event-stream"}}
		if lastSeq > 0 {
			// The header rather than ?after_seq=, because the daemon checks
			// it first and it keeps the URL stable across reconnects.
			header.Set("Last-Event-ID", strconv.FormatInt(lastSeq, 10))
		}

		resp, err := e.client.stream(ctx, execPath(id, "stream"), header)
		if err != nil {
			// A 404 or a 400 will not fix itself; only transport failures
			// are worth retrying.
			var apiErr *apiError
			if errors.As(err, &apiErr) {
				return err
			}
			if ctx.Err() != nil {
				return nil
			}
			if attempt >= maxReconnects {
				return err
			}
		} else {
			seen, err := consume(resp.Body, r, lastSeq)
			resp.Body.Close()
			// Even on a read error: resume from what arrived rather than
			// replaying the whole transcript.
			lastSeq = seen

			switch {
			case errors.Is(err, errTerminal):
				return nil
			case ctx.Err() != nil:
				// Interrupting a follow stops the view, not the run.
				fmt.Fprintf(os.Stderr,
					"\nstopped following; execution %d may still be running\n", id)
				return nil
			}
			// Progress resets the budget, so a long run that blips several
			// times is not cut off.
			attempt, backoff = 0, 250*time.Millisecond
		}

		if attempt >= maxReconnects {
			return fmt.Errorf("stream for execution %d dropped repeatedly", id)
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

// consume reads SSE frames until the stream ends, returning the last
// sequence number seen so a reconnect can resume from it.
func consume(body io.Reader, r *renderer, lastSeq int64) (int64, error) {
	reader := bufio.NewReaderSize(body, 64*1024)
	var data bytes.Buffer

	for {
		line, err := readLine(reader)
		if err != nil {
			return lastSeq, err
		}
		line = bytes.TrimRight(line, "\r\n")

		// A comment is the daemon's keep-alive. It arrives every 20 seconds
		// and must not surface as an empty event.
		if len(line) > 0 && line[0] == ':' {
			continue
		}

		// A blank line dispatches whatever has accumulated.
		if len(line) == 0 {
			if data.Len() == 0 {
				continue
			}
			seq, err := renderFrame(data.Bytes(), r, lastSeq)
			data.Reset()
			if seq > lastSeq {
				lastSeq = seq
			}
			if err != nil {
				return lastSeq, err
			}
			continue
		}

		field, value, _ := bytes.Cut(line, []byte(":"))
		// One optional space after the colon, per the SSE grammar.
		value = bytes.TrimPrefix(value, []byte(" "))
		if string(field) == "data" {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(value)
		}
		// id: and event: are ignored deliberately. The sequence number and
		// the type both ride inside the JSON payload, so reading them from
		// the frame headers would be a second source of truth.
	}
}

// renderFrame decodes one frame's data and renders it.
func renderFrame(data []byte, r *renderer, lastSeq int64) (int64, error) {
	// The frame's payload is exactly what the daemon marshals from an
	// executor.Message, so it is decoded rather than re-modelled.
	var msg executor.Message
	if err := json.Unmarshal(data, &msg); err != nil {
		// A malformed frame is not worth tearing the stream down for.
		return 0, nil
	}

	// Status frames carry no sequence number: one is published when a run
	// starts and one when it finishes, and only the latter is terminal.
	if msg.Type == "status" {
		if msg.Terminal {
			if msg.Status != "" {
				fmt.Fprintf(r.out, "\nstatus: %s\n", msg.Status)
			}
			return 0, errTerminal
		}
		return 0, nil
	}

	// The daemon may replay across the boundary between the backfill and
	// the live feed.
	if msg.Seq != 0 && msg.Seq <= lastSeq {
		return 0, nil
	}
	r.render(msg.Type, msg.Subtype, msg.Payload)
	return msg.Seq, nil
}

// readLine reads one line of any length.
//
// bufio.Scanner is not used here for the same reason internal/claude avoids
// it on the NDJSON stream: a single data field can carry a whole system/init
// payload or a large tool result, well past Scanner's 64 KiB token limit,
// which would abort the stream mid-run.
func readLine(r *bufio.Reader) ([]byte, error) {
	var full []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			full = append(full, chunk...)
			continue
		}
		if err != nil {
			if len(full)+len(chunk) > 0 {
				// A final line with no newline still counts.
				return append(full, chunk...), nil
			}
			return nil, err
		}
		return append(full, chunk...), nil
	}
}
