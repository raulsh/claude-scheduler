package ipc

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func socketPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "test.sock")
}

func TestListenAndServe(t *testing.T) {
	path := socketPath(t)

	ln, err := Listen(path, 0o660)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	if !Serving(path) {
		t.Error("Serving reported false for a bound socket")
	}
	if mode := statMode(t, path); mode != 0o660 {
		t.Errorf("socket mode = %#o, want 0660: the umask must not leak through", mode)
	}
}

// TestListenReplacesStaleSocket covers the crash case: a socket file whose
// process is gone must not need manual cleanup before the service restarts.
func TestListenReplacesStaleSocket(t *testing.T) {
	path := socketPath(t)

	// A socket file with nothing behind it, which is what SIGKILL leaves.
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("prepare stale socket: %v", err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the stale socket should still exist: %v", err)
	}

	ln, err := Listen(path, 0o660)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	defer ln.Close()

	if !Serving(path) {
		t.Error("the replacement socket is not serving")
	}
}

// TestListenRefusesLiveSocket is the other half: taking over a socket a live
// daemon owns would leave two listeners, with clients reaching only one.
func TestListenRefusesLiveSocket(t *testing.T) {
	path := socketPath(t)

	first, err := Listen(path, 0o660)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	defer first.Close()

	second, err := Listen(path, 0o660)
	if err == nil {
		second.Close()
		t.Fatal("a second Listen on a live socket should be refused")
	}
	if !errors.Is(err, ErrAlreadyServing) {
		t.Errorf("error = %v, want ErrAlreadyServing", err)
	}

	// The refusal must not have disturbed the running listener.
	if !Serving(path) {
		t.Error("the first listener stopped serving after the second was refused")
	}
}

func TestCloseUnlinksSocket(t *testing.T) {
	path := socketPath(t)

	ln, err := Listen(path, 0o660)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the socket should be unlinked on close, stat gave %v", err)
	}
	// And the path is immediately reusable.
	again, err := Listen(path, 0o660)
	if err != nil {
		t.Fatalf("Listen after Close: %v", err)
	}
	again.Close()
}

func TestServingIsFalseWhenAbsent(t *testing.T) {
	if Serving(socketPath(t)) {
		t.Error("Serving reported true for a path with no socket")
	}
}

// TestExplainDistinguishesFailures pins the messages apart: "not running"
// and "not allowed" need different remedies.
func TestExplainDistinguishesFailures(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"missing":  {syscall.ENOENT, "no claude-scheduler is listening"},
		"stale":    {syscall.ECONNREFUSED, "nothing is listening"},
		"denied":   {syscall.EACCES, "permission denied"},
		"too long": {syscall.EINVAL, "unusable"},
	}
	for name, c := range cases {
		got := Explain("/run/x.sock", c.err).Error()
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: Explain gave %q, want it to mention %q", name, got, c.want)
		}
		if !Unreachable(c.err) {
			t.Errorf("%s: Unreachable should be true", name)
		}
	}
}

func statMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}
