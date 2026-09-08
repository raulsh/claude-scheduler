// Package api exposes the scheduler's HTTP surface: a JSON API under
// /api/v1, Server-Sent Event streams, and the embedded single-page app.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/raulsh/claude-scheduler/internal/config"
	"github.com/raulsh/claude-scheduler/internal/executor"
	"github.com/raulsh/claude-scheduler/internal/health"
	"github.com/raulsh/claude-scheduler/internal/store"
)

// Server wires dependencies into HTTP handlers.
type Server struct {
	cfg      config.Config
	store    *store.Store
	log      *slog.Logger
	executor *executor.Executor
	runner   Runner
	health   *health.Registry
	logins   *health.LoginManager
	reloader Reloader
}

// Reloader re-synchronises cron registrations after a task changes, so an
// edit takes effect immediately rather than at the next service restart.
type Reloader interface {
	Reload(ctx context.Context) error
}

// Deps bundles the optional collaborators the API serves. A nil field makes
// the corresponding endpoints report unavailable rather than panicking,
// which keeps the service diagnosable when a subsystem fails to start.
type Deps struct {
	Executor *executor.Executor
	Health   *health.Registry
	Logins   *health.LoginManager
	Reloader Reloader
}

// New builds the API server.
func New(cfg config.Config, st *store.Store, log *slog.Logger, deps Deps) *Server {
	s := &Server{
		cfg:      cfg,
		store:    st,
		log:      log,
		executor: deps.Executor,
		health:   deps.Health,
		logins:   deps.Logins,
		reloader: deps.Reloader,
	}
	if deps.Executor != nil {
		s.runner = deps.Executor
	}
	return s
}

// Handler returns the fully routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Tasks.
	mux.HandleFunc("GET /api/v1/tasks", s.handleListTasks)
	mux.HandleFunc("POST /api/v1/tasks", s.handleCreateTask)
	mux.HandleFunc("GET /api/v1/tasks/{id}", s.handleGetTask)
	mux.HandleFunc("PATCH /api/v1/tasks/{id}", s.handleUpdateTask)
	mux.HandleFunc("DELETE /api/v1/tasks/{id}", s.handleDeleteTask)
	mux.HandleFunc("POST /api/v1/tasks/{id}/run", s.handleRunTask)
	mux.HandleFunc("POST /api/v1/tasks/{id}/preflight", s.handleTaskPreflight)
	mux.HandleFunc("POST /api/v1/tasks/{id}/pause", s.handlePauseTask)
	mux.HandleFunc("POST /api/v1/tasks/{id}/resume", s.handleResumeTask)

	// Executions.
	mux.HandleFunc("GET /api/v1/executions", s.handleListExecutions)
	mux.HandleFunc("GET /api/v1/executions/{id}", s.handleGetExecution)
	mux.HandleFunc("GET /api/v1/executions/{id}/events", s.handleListEvents)
	mux.HandleFunc("GET /api/v1/executions/{id}/stream", s.handleStreamExecution)
	mux.HandleFunc("POST /api/v1/executions/{id}/cancel", s.handleCancelExecution)
	mux.HandleFunc("POST /api/v1/executions/{id}/rerun-with-allowlist", s.handleRerunWithAllowlist)

	// Health of the scheduler itself, and of the dependencies it guards.
	mux.HandleFunc("GET /api/v1/health", s.handleServiceHealth)
	mux.HandleFunc("GET /api/v1/health/checks", s.handleListChecks)
	mux.HandleFunc("GET /api/v1/health/snapshot", s.handleHealthSnapshot)
	mux.HandleFunc("GET /api/v1/health/kinds", s.handleListKinds)
	mux.HandleFunc("GET /api/v1/health/{kind}", s.handleCheckKind)
	mux.HandleFunc("GET /api/v1/health/{kind}/{target}", s.handleCheckOne)
	mux.HandleFunc("POST /api/v1/health/aws/{profile}/login", s.handleStartAWSLogin)
	mux.HandleFunc("GET /api/v1/health/aws/{profile}/login", s.handleGetAWSLogin)
	mux.HandleFunc("DELETE /api/v1/health/aws/{profile}/login", s.handleCancelAWSLogin)

	// Key/value scratch space for runs. {key...} is a multi-segment
	// wildcard so a slash-separated key stays one key.
	mux.HandleFunc("GET /api/v1/kv", s.handleListKV)
	mux.HandleFunc("GET /api/v1/kv/{key...}", s.handleGetKV)
	mux.HandleFunc("PUT /api/v1/kv/{key...}", s.handleSetKV)
	mux.HandleFunc("DELETE /api/v1/kv/{key...}", s.handleDeleteKV)

	// The SPA owns every remaining path.
	mux.Handle("/", s.spaHandler())

	return s.withMiddleware(mux)
}

// withMiddleware applies panic recovery.
//
// There is no authentication layer: the API is reachable only through a Unix
// socket, and that socket's file permissions are the access control. A
// bearer token here would have to be shared with every CLI invocation and
// with the browser, which never sent one, while adding nothing that the
// socket permissions do not already enforce.
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return s.recoverPanics(next)
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic serving request", "method", r.Method, "path", r.URL.Path, "panic", v)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// pathID parses the {id} path segment.
func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already sent, so there is nothing to do but note it.
		slog.Debug("encode response", "error", err)
	}
}

type errorBody struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

// writeStoreError maps store errors onto status codes.
func writeStoreError(w http.ResponseWriter, err error, action string) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeError(w, http.StatusInternalServerError, action+": "+err.Error())
}

// decodeJSON reads a request body, rejecting unknown fields so a typo in a
// client payload surfaces instead of being silently dropped.
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// reloadSchedules asks the scheduler to pick up a task change. A failure is
// logged rather than surfaced: the write already succeeded, and the next
// periodic reload will converge.
func (s *Server) reloadSchedules(ctx context.Context) {
	if s.reloader == nil {
		return
	}
	if err := s.reloader.Reload(ctx); err != nil {
		s.log.Warn("could not reload schedules after a task change", "error", err)
	}
}
