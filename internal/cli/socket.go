package cli

import (
	"os"

	"gopkg.in/yaml.v3"

	"github.com/raulsh/claude-scheduler/internal/config"
)

// resolveSocket picks the socket to talk to.
//
// The environment sits above the config file because that is how a running
// execution finds the daemon: the executor exports CLAUDE_SCHEDULER_SOCKET,
// so a task's prompt can call this CLI with no flags and no readable config.
func resolveSocket(flagValue, configPath string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv("CLAUDE_SCHEDULER_SOCKET"); v != "" {
		return v
	}
	if path, ok := socketFromConfig(configPath); ok {
		return path
	}
	return config.DefaultSocket
}

// socketFromConfig reads server.socket, and only that, out of the daemon's
// configuration file.
//
// config.Load is deliberately not used here. It runs Validate, so an
// unrelated bad field, a Slack webhook or a negative TTL, would stop the CLI
// from reaching a perfectly healthy daemon. It also resolves binary paths
// through exec.LookPath, which would make the answer depend on the client's
// PATH rather than the daemon's. And a config this user cannot read is not
// an error: the daemon's configuration belongs to the daemon, and someone
// who cannot read it can still be allowed to use the socket.
func socketFromConfig(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}

	var doc struct {
		Server struct {
			Socket string `yaml:"socket"`
		} `yaml:"server"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", false
	}
	return doc.Server.Socket, doc.Server.Socket != ""
}
