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
	if cfg.Server.Port != 9977 {
		t.Errorf("Port = %d, want the default 9977", cfg.Server.Port)
	}
}

func TestNonLoopbackRequiresToken(t *testing.T) {
	path := writeConfig(t, "server:\n  bind: 0.0.0.0\n  port: 9977\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("binding beyond loopback without a token should be rejected")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error should explain the token requirement: %v", err)
	}

	// With a token it is allowed.
	path = writeConfig(t, "server:\n  bind: 0.0.0.0\n  port: 9977\n  token: secret\n")
	if _, err := Load(path); err != nil {
		t.Errorf("a non-loopback bind with a token should be accepted: %v", err)
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
	path := writeConfig(t, "server:\n  port: 1234\n")

	t.Setenv("CLAUDE_SCHEDULER_PORT", "4321")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 4321 {
		t.Errorf("Port = %d, want the environment override 4321", cfg.Server.Port)
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
