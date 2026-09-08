// Package config loads claude-scheduler configuration from defaults, a YAML
// file, and the environment, in that order of increasing precedence.
package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	Server    Server    `yaml:"server"`
	Paths     Paths     `yaml:"paths"`
	Binaries  Binaries  `yaml:"binaries"`
	Executor  Executor  `yaml:"executor"`
	Health    Health    `yaml:"health"`
	Notify    Notify    `yaml:"notify"`
	Retention Retention `yaml:"retention"`
}

type Server struct {
	// Socket is the Unix socket the daemon serves the API on. Access control
	// is the socket's file permissions: anything that can write to it can
	// start a run, so there is no token to configure.
	Socket string `yaml:"socket"`
	// SocketMode is applied to the socket file after binding, because the
	// kernel would otherwise subtract the daemon's umask from 0777.
	SocketMode FileMode `yaml:"socket_mode"`

	// Bind and Port are no longer used: the daemon does not listen on a TCP
	// port at all, and `claude-scheduler proxy` exposes one on demand. They
	// are kept only so that a config file carried over from an earlier
	// version can be warned about rather than silently ignored.
	Bind string `yaml:"bind"`
	Port int    `yaml:"port"`
}

// LegacyTCPKeys reports whether the config still asks for a TCP listener.
func (s Server) LegacyTCPKeys() bool { return s.Bind != "" || s.Port != 0 }

// DefaultSocket is where the packaged service listens. systemd creates the
// parent through RuntimeDirectory= and removes it on stop, which also clears
// the socket.
const DefaultSocket = "/run/claude-scheduler/scheduler.sock"

// maxSocketPath is the usable length of a Unix socket path. sun_path is a
// fixed 108-byte field including its terminator, and a path that overruns it
// fails at bind with a bare "invalid argument" that says nothing about
// length, so it is worth rejecting with an explanation instead.
const maxSocketPath = 104

// Deprecations lists retired settings still present in the file, so an
// upgraded install is told why they stopped working instead of finding out
// when the web UI no longer answers. yaml.v3 ignores unknown keys, so
// without the vestigial fields these would vanish without a word.
func (c Config) Deprecations() []string {
	var out []string
	if c.Server.LegacyTCPKeys() {
		out = append(out, "server.bind and server.port are ignored: the daemon listens "+
			"only on server.socket. To serve a TCP port, run `claude-scheduler proxy`.")
	}
	return out
}

// ValidateSocketPath rejects socket paths that cannot work, with a reason.
func ValidateSocketPath(path string) error {
	switch {
	case path == "":
		return errors.New("is empty")
	case !filepath.IsAbs(path):
		// systemd runs the service with / as its working directory, so a
		// relative path would resolve somewhere nobody intended.
		return fmt.Errorf("%q is not an absolute path", path)
	case len(path) > maxSocketPath:
		return fmt.Errorf("%q is %d bytes; a unix socket path cannot exceed %d",
			path, len(path), maxSocketPath)
	}
	return nil
}

type Paths struct {
	DataDir string `yaml:"data_dir"`
	LogDir  string `yaml:"log_dir"`
}

// DBPath is the SQLite database location, derived from DataDir.
func (p Paths) DBPath() string { return filepath.Join(p.DataDir, "scheduler.db") }

// TranscriptDir holds the raw NDJSON stream tee'd from each execution.
func (p Paths) TranscriptDir() string { return filepath.Join(p.LogDir, "executions") }

type Binaries struct {
	Claude string `yaml:"claude"`
	AWS    string `yaml:"aws"`
}

type Executor struct {
	MaxConcurrent    int      `yaml:"max_concurrent"`
	DefaultTimeout   Duration `yaml:"default_timeout"`
	DefaultMaxBudget float64  `yaml:"default_max_budget_usd"`
	// MisfireGrace bounds how stale a missed cron occurrence may be and still
	// run once the service comes back. Zero disables catch-up entirely.
	MisfireGrace Duration `yaml:"misfire_grace"`
}

type Health struct {
	AWSTTL    Duration `yaml:"aws_ttl"`
	MCPTTL    Duration `yaml:"mcp_ttl"`
	BinaryTTL Duration `yaml:"binary_ttl"`
	// CheckTimeout bounds a single probe. `claude mcp list` can take ~60s.
	CheckTimeout Duration `yaml:"check_timeout"`
}

type Notify struct {
	Desktop DesktopNotify `yaml:"desktop"`
	Slack   SlackNotify   `yaml:"slack"`
}

type DesktopNotify struct {
	Enabled bool `yaml:"enabled"`
}

type SlackNotify struct {
	Enabled    bool   `yaml:"enabled"`
	WebhookURL string `yaml:"webhook_url"`
}

type Retention struct {
	TranscriptDays int `yaml:"transcript_days"`
	ExecutionDays  int `yaml:"execution_days"`
}

// Default returns the baseline configuration before file and env overrides.
func Default() Config {
	return Config{
		Server: Server{
			Socket:     DefaultSocket,
			SocketMode: 0o660,
		},
		Paths: Paths{
			DataDir: "/var/lib/claude-scheduler",
			LogDir:  "/var/log/claude-scheduler",
		},
		Binaries: Binaries{Claude: "", AWS: ""},
		Executor: Executor{
			MaxConcurrent:    2,
			DefaultTimeout:   Duration(30 * time.Minute),
			DefaultMaxBudget: 1.0,
			MisfireGrace:     Duration(15 * time.Minute),
		},
		Health: Health{
			AWSTTL:       Duration(5 * time.Minute),
			MCPTTL:       Duration(2 * time.Minute),
			BinaryTTL:    Duration(10 * time.Minute),
			CheckTimeout: Duration(90 * time.Second),
		},
		Notify:    Notify{Desktop: DesktopNotify{Enabled: true}},
		Retention: Retention{TranscriptDays: 30, ExecutionDays: 365},
	}
}

// Load resolves configuration from path (optional) then the environment.
// A missing file is not an error: the defaults are a working configuration.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return cfg, fmt.Errorf("parse %s: %w", path, err)
			}
		case errors.Is(err, os.ErrNotExist):
			// Defaults stand.
		default:
			return cfg, fmt.Errorf("read %s: %w", path, err)
		}
	}

	applyEnv(&cfg)
	cfg.resolveBinaries()

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// applyEnv overlays CLAUDE_SCHEDULER_* variables. Only the settings worth
// overriding per-invocation are wired up; the file covers the rest.
func applyEnv(cfg *Config) {
	if v := os.Getenv("CLAUDE_SCHEDULER_SOCKET"); v != "" {
		cfg.Server.Socket = v
	}
	if v := os.Getenv("CLAUDE_SCHEDULER_DATA_DIR"); v != "" {
		cfg.Paths.DataDir = v
	}
	if v := os.Getenv("CLAUDE_SCHEDULER_LOG_DIR"); v != "" {
		cfg.Paths.LogDir = v
	}
	if v := os.Getenv("CLAUDE_SCHEDULER_CLAUDE_BIN"); v != "" {
		cfg.Binaries.Claude = v
	}
	if v := os.Getenv("CLAUDE_SCHEDULER_AWS_BIN"); v != "" {
		cfg.Binaries.AWS = v
	}
}

// resolveBinaries fills in unset binary paths. The claude CLI installs
// per-user outside /usr/bin, so PATH lookup is tried first and then the
// conventional install location under the service user's home.
func (c *Config) resolveBinaries() {
	if c.Binaries.Claude == "" {
		c.Binaries.Claude = lookBinary("claude", filepath.Join(os.Getenv("HOME"), ".local", "bin", "claude"))
	}
	if c.Binaries.AWS == "" {
		c.Binaries.AWS = lookBinary("aws", "/usr/local/bin/aws")
	}
}

func lookBinary(name string, fallbacks ...string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, f := range fallbacks {
		if f == "" {
			continue
		}
		if st, err := os.Stat(f); err == nil && !st.IsDir() {
			return f
		}
	}
	return ""
}

// Validate rejects configurations that cannot produce a working service.
func (c Config) Validate() error {
	var problems []string

	if err := ValidateSocketPath(c.Server.Socket); err != nil {
		problems = append(problems, "server.socket: "+err.Error())
	}
	// The socket's permissions are the API's only access control, so a mode
	// that lets anyone connect is a hard error rather than a warning.
	// Connecting to a Unix socket requires the write bit, which is why
	// other-write is the dangerous one: it would let any local user start
	// runs as the service user.
	switch mode := c.Server.SocketMode; {
	case mode&0o002 != 0:
		problems = append(problems, fmt.Sprintf(
			"server.socket_mode %s grants write to other, which would let any local "+
				"user start runs as this user; use 0660 or 0600", mode))
	case mode&0o600 != 0o600:
		problems = append(problems, fmt.Sprintf(
			"server.socket_mode %s does not grant the owner read and write, so the "+
				"daemon could not use its own socket", mode))
	}
	if c.Paths.DataDir == "" {
		problems = append(problems, "paths.data_dir is empty")
	}
	if c.Paths.LogDir == "" {
		problems = append(problems, "paths.log_dir is empty")
	}
	if c.Executor.MaxConcurrent < 1 {
		problems = append(problems, "executor.max_concurrent must be at least 1")
	}
	if c.Executor.DefaultTimeout <= 0 {
		problems = append(problems, "executor.default_timeout must be positive")
	}
	if c.Executor.MisfireGrace < 0 {
		problems = append(problems, "executor.misfire_grace cannot be negative (use 0 to disable catch-up)")
	}
	if c.Health.CheckTimeout <= 0 {
		problems = append(problems, "health.check_timeout must be positive")
	}
	for name, ttl := range map[string]Duration{
		"health.aws_ttl":    c.Health.AWSTTL,
		"health.mcp_ttl":    c.Health.MCPTTL,
		"health.binary_ttl": c.Health.BinaryTTL,
	} {
		if ttl < 0 {
			problems = append(problems, name+" cannot be negative")
		}
	}
	if c.Notify.Slack.Enabled && c.Notify.Slack.WebhookURL == "" {
		problems = append(problems, "notify.slack.enabled requires notify.slack.webhook_url")
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}
