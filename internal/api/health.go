package api

import (
	"net/http"
	"runtime"
	"time"

	"github.com/raulsh/claude-scheduler/internal/webui"
)

// Version is set at build time via -ldflags.
var Version = "dev"

var startedAt = time.Now()

// serviceHealth describes the scheduler process itself, as distinct from the
// dependency healthchecks it runs on behalf of tasks.
type serviceHealth struct {
	Status     string       `json:"status"`
	Version    string       `json:"version"`
	UptimeSecs int64        `json:"uptime_seconds"`
	GoVersion  string       `json:"go_version"`
	Database   string       `json:"database"`
	UIBuilt    bool         `json:"ui_built"`
	Binaries   healthBinary `json:"binaries"`
	Warnings   []string     `json:"warnings"`
}

type healthBinary struct {
	Claude string `json:"claude"`
	AWS    string `json:"aws"`
}

func (s *Server) handleServiceHealth(w http.ResponseWriter, r *http.Request) {
	h := serviceHealth{
		Status:     "ok",
		Version:    Version,
		UptimeSecs: int64(time.Since(startedAt).Seconds()),
		GoVersion:  runtime.Version(),
		Database:   s.store.Path(),
		UIBuilt:    webui.Built(),
		Binaries:   healthBinary{Claude: s.cfg.Binaries.Claude, AWS: s.cfg.Binaries.AWS},
		Warnings:   []string{},
	}

	if err := s.store.Ping(r.Context()); err != nil {
		h.Status = "degraded"
		h.Warnings = append(h.Warnings, "database unreachable: "+err.Error())
	}

	// The claude CLI installs per-user outside /usr/bin, so a missing path is
	// the single most likely reason a fresh install cannot run anything.
	if s.cfg.Binaries.Claude == "" {
		h.Status = "degraded"
		h.Warnings = append(h.Warnings,
			"the claude CLI was not found; set binaries.claude in /etc/claude-scheduler/config.yaml")
	}
	if s.cfg.Binaries.AWS == "" {
		h.Warnings = append(h.Warnings,
			"the aws CLI was not found; AWS profile checks will report unavailable")
	}
	if !webui.Built() {
		h.Warnings = append(h.Warnings, "the web UI has not been built; run make ui")
	}

	status := http.StatusOK
	if h.Status != "ok" {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, h)
}

// handleListChecks returns the most recent result for each dependency the
// scheduler knows about.
func (s *Server) handleListChecks(w http.ResponseWriter, r *http.Request) {
	checks, err := s.store.LatestChecks(r.Context())
	if err != nil {
		writeStoreError(w, err, "list checks")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checks": orEmpty(checks)})
}
