package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/raulsh/claude-scheduler/internal/ipc"
)

// client talks to the daemon over its Unix socket. It is a thin wrapper
// around the same HTTP API the web UI uses, so there is one contract rather
// than two.
type client struct {
	socket string
	http   *http.Client
}

func newClient(socket string) *client {
	return &client{socket: socket, http: ipc.Client(socket)}
}

// apiError carries a failed response so callers can map a status onto an
// exit code without re-reading the body.
type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

// do issues a request and returns the response body, or an *apiError.
func (c *client) do(ctx context.Context, method, path string, body io.Reader, contentType string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, ipc.URL(path), body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, ipc.Explain(c.socket, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return nil, &apiError{status: resp.StatusCode, msg: describe(resp.StatusCode, payload)}
	}
	return payload, nil
}

// stream issues a request and hands back the live response for the caller to
// read incrementally. Used for SSE, where reading to completion would mean
// waiting for the run to end.
func (c *client) stream(ctx context.Context, path string, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ipc.URL(path), nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, ipc.Explain(c.socket, err)
	}
	if resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &apiError{status: resp.StatusCode, msg: describe(resp.StatusCode, payload)}
	}
	return resp, nil
}

// getJSON decodes a JSON response into dst.
func (c *client) getJSON(ctx context.Context, path string, dst any) error {
	payload, err := c.do(ctx, http.MethodGet, path, nil, "")
	if err != nil {
		return err
	}
	return decode(payload, dst)
}

// sendJSON issues a request with a JSON body, decoding the response into dst
// when dst is non-nil.
func (c *client) sendJSON(ctx context.Context, method, path string, body, dst any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}

	payload, err := c.do(ctx, method, path, reader, "application/json")
	if err != nil {
		return err
	}
	if dst == nil {
		return nil
	}
	return decode(payload, dst)
}

func decode(payload []byte, dst any) error {
	if len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, dst); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// describe turns an error response into a message. The API answers with
// {"error":...} and sometimes {"problems":[...]}, so both are unwrapped
// rather than shown as raw JSON.
func describe(status int, payload []byte) string {
	var body struct {
		Error    string   `json:"error"`
		Problems []string `json:"problems"`
	}
	if err := json.Unmarshal(payload, &body); err != nil || body.Error == "" {
		if len(payload) > 0 {
			return fmt.Sprintf("%s: %s", http.StatusText(status), bytes.TrimSpace(payload))
		}
		return http.StatusText(status)
	}

	msg := body.Error
	for _, p := range body.Problems {
		msg += "\n  - " + p
	}
	return msg
}

// jsonBody issues a request with a JSON body and returns the raw response.
func (c *client) jsonBody(ctx context.Context, method, path string, body any) ([]byte, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, method, path, bytes.NewReader(encoded), "application/json")
}
