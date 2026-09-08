package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestDurationForms covers every way someone might reasonably write a
// duration by hand. A bare 0 is the one that used to fail outright.
func TestDurationForms(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want time.Duration
	}{
		{"bare zero", "misfire_grace: 0", 0},
		{"zero with unit", "misfire_grace: 0s", 0},
		{"quoted zero with unit", `misfire_grace: "0s"`, 0},
		{"minutes", "misfire_grace: 15m", 15 * time.Minute},
		{"hours and minutes", "misfire_grace: 1h30m", 90 * time.Minute},
		{"bare number is seconds", "misfire_grace: 90", 90 * time.Second},
		{"fractional seconds", "misfire_grace: 1.5", 1500 * time.Millisecond},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, "executor:\n  "+tc.yaml+"\n")
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load(%q): %v", tc.yaml, err)
			}
			if got := cfg.Executor.MisfireGrace.Std(); got != tc.want {
				t.Errorf("MisfireGrace = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEmptyValueKeepsDefault documents the deliberate choice: a key written
// with no value leaves the default in place rather than zeroing the setting.
// Silently disabling catch-up because someone left a line half-edited would
// be a nasty surprise.
func TestEmptyValueKeepsDefault(t *testing.T) {
	path := writeConfig(t, "executor:\n  misfire_grace:\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Executor.MisfireGrace.Std(); got != 15*time.Minute {
		t.Errorf("MisfireGrace = %v, want the default 15m", got)
	}
}

func TestDurationRejectsNonsense(t *testing.T) {
	path := writeConfig(t, "executor:\n  misfire_grace: not-a-duration\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for an unparseable duration")
	}
	// The message should say what a valid value looks like.
	if !strings.Contains(err.Error(), "15m") {
		t.Errorf("error does not show the expected form: %v", err)
	}
}

func TestDefaultsAreValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("the built-in defaults do not validate: %v", err)
	}
}

func TestMissingFileFallsBackToDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("a missing config should not be an error: %v", err)
	}
	if cfg.Server.Socket != DefaultSocket {
		t.Errorf("Socket = %q, want the default %q", cfg.Server.Socket, DefaultSocket)
	}
}

// TestRejectsWorldWritableSocketMode guards the access-control story:
// connecting to a Unix socket needs the write bit, so other-write would let
// any local user start runs as the service user.
func TestRejectsWorldWritableSocketMode(t *testing.T) {
	path := writeConfig(t, "server:\n  socket_mode: 0666\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("a world-writable socket mode should be rejected")
	}
	if !strings.Contains(err.Error(), "socket_mode") {
		t.Errorf("error should name the field: %v", err)
	}
}

func TestRejectsRelativeSocket(t *testing.T) {
	path := writeConfig(t, "server:\n  socket: ./scheduler.sock\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("a relative socket path should be rejected")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error should explain the requirement: %v", err)
	}
}

// TestRejectsOverlongSocket covers the sun_path limit, whose native failure
// is a bare "invalid argument" from bind that explains nothing.
func TestRejectsOverlongSocket(t *testing.T) {
	long := "/" + strings.Repeat("a", 120) + ".sock"
	path := writeConfig(t, "server:\n  socket: "+long+"\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("an overlong socket path should be rejected")
	}
	if !strings.Contains(err.Error(), "unix socket path") {
		t.Errorf("error should explain the length limit: %v", err)
	}
}

// TestSocketModeSpellings pins the octal reading. yaml.v3 would resolve a
// bare 660 as decimal, which is mode 01224 and not what anyone means.
func TestSocketModeSpellings(t *testing.T) {
	for _, spelling := range []string{"0660", "0o660", "660", `"0660"`} {
		path := writeConfig(t, "server:\n  socket_mode: "+spelling+"\n")
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("socket_mode: %s: %v", spelling, err)
		}
		if cfg.Server.SocketMode != 0o660 {
			t.Errorf("socket_mode: %s parsed as %s, want 0660", spelling, cfg.Server.SocketMode)
		}
	}
}

// TestLegacyTCPKeysAreReported guards the upgrade path: yaml.v3 ignores
// unknown keys, so a carried-over bind/port would otherwise vanish silently.
func TestLegacyTCPKeysAreReported(t *testing.T) {
	path := writeConfig(t, "server:\n  bind: 0.0.0.0\n  port: 9977\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("a config with retired keys should still load: %v", err)
	}
	notes := cfg.Deprecations()
	if len(notes) == 0 {
		t.Fatal("server.bind and server.port should be reported as obsolete")
	}
	if !strings.Contains(notes[0], "proxy") {
		t.Errorf("the note should point at the proxy command: %q", notes[0])
	}
}

func TestNegativeDurationsRejected(t *testing.T) {
	path := writeConfig(t, "executor:\n  misfire_grace: -5m\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("a negative misfire grace should be rejected")
	}
	if !strings.Contains(err.Error(), "misfire_grace") {
		t.Errorf("error should name the field: %v", err)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	path := writeConfig(t, "server:\n  socket: /run/from-file.sock\n")

	t.Setenv("CLAUDE_SCHEDULER_SOCKET", "/run/from-env.sock")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Socket != "/run/from-env.sock" {
		t.Errorf("Socket = %q, want the environment override", cfg.Server.Socket)
	}
}

// TestPartialConfigKeepsDefaults guards the merge behaviour: a config that
// mentions one field must not zero the rest.
func TestPartialConfigKeepsDefaults(t *testing.T) {
	path := writeConfig(t, "executor:\n  max_concurrent: 4\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Executor.MaxConcurrent != 4 {
		t.Errorf("MaxConcurrent = %d, want 4", cfg.Executor.MaxConcurrent)
	}
	if cfg.Executor.DefaultTimeout.Std() != 30*time.Minute {
		t.Errorf("DefaultTimeout = %v, want the default 30m", cfg.Executor.DefaultTimeout)
	}
	if cfg.Health.MCPTTL.Std() != 2*time.Minute {
		t.Errorf("MCPTTL = %v, want the default 2m", cfg.Health.MCPTTL)
	}
}
