package api

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raulsh/claude-scheduler/internal/config"
	"github.com/raulsh/claude-scheduler/internal/store"
)

// newKVServer builds the real handler over a real database. The zero Deps is
// fine: the KV endpoints use neither the executor nor the health registry.
func newKVServer(t *testing.T) *httptest.Server {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(New(config.Default(), st, log, Deps{}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func put(t *testing.T, srv *httptest.Server, path string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, srv.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", path, err)
	}
	return resp
}

// TestKVRouteBeatsSPACatchAll pins the routing. The mux tries literal
// segments before wildcards, so /api/v1/kv/a/b/c reaches the KV handler
// rather than the SPA's "/" catch-all, and the whole remainder arrives as
// one key. Reordering Handler() or renaming the wildcard breaks this
// silently otherwise.
func TestKVRouteBeatsSPACatchAll(t *testing.T) {
	srv := newKVServer(t)

	resp := put(t, srv, "/api/v1/kv/a/b/c", []byte("nested"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", resp.StatusCode)
	}

	got, err := srv.Client().Get(srv.URL + "/api/v1/kv/a/b/c")
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()

	body, _ := io.ReadAll(got.Body)
	if string(body) != "nested" {
		t.Errorf("body = %q, want the stored value; the key was probably not %q", body, "a/b/c")
	}
}

// TestKVValueIsByteExact is the contract that lets `kv set --file` and
// `kv get` round-trip an arbitrary file.
func TestKVValueIsByteExact(t *testing.T) {
	srv := newKVServer(t)

	// Bytes that would not survive being treated as text or as JSON.
	value := []byte("100% \x00\xff\xfe done\n")
	resp := put(t, srv, "/api/v1/kv/blob", value)
	resp.Body.Close()

	got, err := srv.Client().Get(srv.URL + "/api/v1/kv/blob")
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()

	body, _ := io.ReadAll(got.Body)
	if !bytes.Equal(body, value) {
		t.Errorf("body = %v, want %v", body, value)
	}
	if ct := got.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}
}

// TestKVRejectsTraversalKeys guards a silent data loss. ServeMux cleans the
// request path before matching and redirects when cleaning changed it, and
// Go's HTTP client turns that 301 into a GET, so a key with a "." or ".."
// segment would convert a PUT into a successful-looking read that stored
// nothing at all.
func TestKVRejectsTraversalKeys(t *testing.T) {
	srv := newKVServer(t)

	for _, key := range []string{"a/../b", "a/./b", "a//b"} {
		req, err := http.NewRequest(http.MethodPut, srv.URL+"/api/v1/kv/"+key, strings.NewReader("v"))
		if err != nil {
			t.Fatal(err)
		}
		// Without a redirect the server sees the raw path and rejects it;
		// following one would mask the problem being tested.
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			t.Errorf("PUT with key %q returned 200; a traversal key must not be stored", key)
		}
	}
}

func TestKVEmptyKeyIsBadRequest(t *testing.T) {
	srv := newKVServer(t)

	resp, err := srv.Client().Get(srv.URL + "/api/v1/kv/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: an empty key is reachable through {key...}", resp.StatusCode)
	}
}

func TestKVOversizeValueIsRejected(t *testing.T) {
	srv := newKVServer(t)

	resp := put(t, srv, "/api/v1/kv/big", bytes.Repeat([]byte("x"), maxKVValue+1))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

func TestKVMissingKeyIsNotFound(t *testing.T) {
	srv := newKVServer(t)

	resp, err := srv.Client().Get(srv.URL + "/api/v1/kv/absent")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestParseTTLAcceptsDays covers the one extension over
// time.ParseDuration, which stops at hours: "7d" is the obvious thing to
// type for a value meant to last a week.
func TestParseTTLAcceptsDays(t *testing.T) {
	cases := map[string]time.Duration{
		"30m":  30 * time.Minute,
		"24h":  24 * time.Hour,
		"1d":   24 * time.Hour,
		"7d":   7 * 24 * time.Hour,
		"1.5d": 36 * time.Hour,
	}
	for raw, want := range cases {
		got, err := ParseTTL(raw)
		if err != nil {
			t.Fatalf("ParseTTL(%q): %v", raw, err)
		}
		if got != want {
			t.Errorf("ParseTTL(%q) = %v, want %v", raw, got, want)
		}
	}
	for _, raw := range []string{"soon", "7days", "d", ""} {
		if _, err := ParseTTL(raw); err == nil && raw != "" {
			t.Errorf("ParseTTL(%q) should have failed", raw)
		}
	}
}
