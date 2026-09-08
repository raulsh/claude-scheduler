package api

import (
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/raulsh/claude-scheduler/internal/store"
)

// handleHealthSnapshot returns the current state of every discoverable
// dependency, grouped by kind. ?force=1 bypasses the TTL cache.
func (s *Server) handleHealthSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.health == nil {
		writeError(w, http.StatusServiceUnavailable, "healthchecks are not available")
		return
	}

	force := isTruthy(r.URL.Query().Get("force"))
	snapshot := s.health.Snapshot(r.Context(), force)

	usage, err := s.store.DependencyUsage(r.Context())
	if err != nil {
		s.log.Warn("could not load dependency usage", "error", err)
		usage = map[string]int{}
	}

	// Two counts, because they answer different questions. gating_failures
	// is every unhealthy dependency on this machine; blocking_declared is
	// the subset some enabled task actually requires. Only the latter
	// belongs in a warning banner: a machine can carry dozens of configured
	// MCP connectors no schedule references, and counting those would turn
	// the warning into noise nobody reads.
	counts := map[string]int{}
	gating, declared := 0, 0
	for _, results := range snapshot {
		for _, res := range results {
			counts[res.State]++
			if !res.Gates() {
				continue
			}
			gating++
			if usage[store.DependencyKey(res.Kind, res.Target)] > 0 {
				declared++
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"kinds":             snapshot,
		"counts":            counts,
		"gating_failures":   gating,
		"blocking_declared": declared,
	})
}

// handleCheckKind probes every target of one dependency kind.
//
// The kinds are served separately rather than only as one snapshot because
// they differ enormously in cost: AWS profiles and binaries answer in about
// a second, while the MCP probe health-checks every configured server over
// the network and can take a minute. Behind a single request the fast kinds
// would be invisible until the slow one finished.
func (s *Server) handleCheckKind(w http.ResponseWriter, r *http.Request) {
	if s.health == nil {
		writeError(w, http.StatusServiceUnavailable, "healthchecks are not available")
		return
	}

	kind := r.PathValue("kind")
	checker, known := s.health.Checker(kind)
	if !known {
		writeError(w, http.StatusNotFound,
			"unknown check kind "+kind+"; expected one of "+strings.Join(s.health.Kinds(), ", "))
		return
	}

	force := isTruthy(r.URL.Query().Get("force"))

	targets, err := checker.Targets(r.Context())
	if err != nil {
		// Failing to enumerate is itself reportable state, not a 500: the
		// aws CLI may simply not be installed.
		writeJSON(w, http.StatusOK, map[string]any{
			"kind":    kind,
			"results": []any{},
			"error":   err.Error(),
		})
		return
	}

	results := s.health.CheckAll(r.Context(), kind, targets, force)

	usage, err := s.store.DependencyUsage(r.Context())
	if err != nil {
		s.log.Warn("could not load dependency usage", "error", err)
		usage = map[string]int{}
	}

	gating, declared := 0, 0
	used := make(map[string]int, len(results))
	for _, res := range results {
		if n := usage[store.DependencyKey(res.Kind, res.Target)]; n > 0 {
			used[res.Target] = n
		}
		if !res.Gates() {
			continue
		}
		gating++
		if usage[store.DependencyKey(res.Kind, res.Target)] > 0 {
			declared++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"kind":              kind,
		"results":           results,
		"gating_failures":   gating,
		"blocking_declared": declared,
		"required_by":       used,
	})
}

// handleCheckOne probes a single dependency.
func (s *Server) handleCheckOne(w http.ResponseWriter, r *http.Request) {
	if s.health == nil {
		writeError(w, http.StatusServiceUnavailable, "healthchecks are not available")
		return
	}

	kind := r.PathValue("kind")
	target, err := url.PathUnescape(r.PathValue("target"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid target")
		return
	}
	if _, known := s.health.Checker(kind); !known {
		writeError(w, http.StatusNotFound,
			"unknown check kind "+kind+"; expected one of "+strings.Join(s.health.Kinds(), ", "))
		return
	}

	res := s.health.Check(r.Context(), kind, target, isTruthy(r.URL.Query().Get("force")))
	writeJSON(w, http.StatusOK, res)
}

// handleTaskPreflight runs a task's checks without executing it, so the user
// can confirm a fix took effect before the next scheduled run.
func (s *Server) handleTaskPreflight(w http.ResponseWriter, r *http.Request) {
	if s.health == nil {
		writeError(w, http.StatusServiceUnavailable, "healthchecks are not available")
		return
	}

	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid task id")
		return
	}
	task, err := s.store.GetTask(r.Context(), id)
	if err != nil {
		writeStoreError(w, err, "get task")
		return
	}

	all, failing := s.health.CheckRequirements(r.Context(), task.Requirements, true)

	writeJSON(w, http.StatusOK, map[string]any{
		"task_id":       task.ID,
		"results":       orEmpty(all),
		"failing":       orEmpty(failing),
		"would_run":     len(failing) == 0,
		"gating_policy": task.GatingPolicy,
	})
}

// handleStartAWSLogin begins an SSO login the service drives itself, and
// returns the verification details for the user to complete in a browser.
func (s *Server) handleStartAWSLogin(w http.ResponseWriter, r *http.Request) {
	if s.logins == nil {
		writeError(w, http.StatusServiceUnavailable, "AWS login is not available")
		return
	}

	profile, err := url.PathUnescape(r.PathValue("profile"))
	if err != nil || profile == "" {
		writeError(w, http.StatusBadRequest, "invalid profile")
		return
	}

	session, err := s.logins.Start(profile)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	s.log.Info("aws sso login requested", "profile", profile)
	writeJSON(w, http.StatusAccepted, session)
}

// handleGetAWSLogin reports a login attempt's progress. The UI polls this
// while the user authorises in their browser.
func (s *Server) handleGetAWSLogin(w http.ResponseWriter, r *http.Request) {
	if s.logins == nil {
		writeError(w, http.StatusServiceUnavailable, "AWS login is not available")
		return
	}

	profile, err := url.PathUnescape(r.PathValue("profile"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid profile")
		return
	}

	session, found := s.logins.Get(profile)
	if !found {
		writeError(w, http.StatusNotFound, "no login attempt for profile "+profile)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

// handleCancelAWSLogin aborts an in-flight login.
func (s *Server) handleCancelAWSLogin(w http.ResponseWriter, r *http.Request) {
	if s.logins == nil {
		writeError(w, http.StatusServiceUnavailable, "AWS login is not available")
		return
	}

	profile, err := url.PathUnescape(r.PathValue("profile"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid profile")
		return
	}
	if !s.logins.Cancel(profile) {
		writeError(w, http.StatusConflict, "no login attempt is in flight for "+profile)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"cancelled": profile})
}

// handleResumeTask clears a pause, typically after the dependency that
// caused it has been repaired.
func (s *Server) handleResumeTask(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid task id")
		return
	}
	if _, err := s.store.GetTask(r.Context(), id); err != nil {
		writeStoreError(w, err, "get task")
		return
	}
	if err := s.store.SetTaskPaused(r.Context(), id, false, ""); err != nil {
		writeStoreError(w, err, "resume task")
		return
	}

	s.reloadSchedules(r.Context())
	s.log.Info("task resumed", "task", id)
	task, err := s.store.GetTask(r.Context(), id)
	if err != nil {
		writeStoreError(w, err, "get task")
		return
	}
	if next, err := nextRun(*task); err == nil && !next.IsZero() {
		task.NextRunAt = &next
	}
	writeJSON(w, http.StatusOK, task)
}

func isTruthy(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// handleListKinds reports which dependency kinds this build can check, so
// the UI can render one section per kind and load them independently.
func (s *Server) handleListKinds(w http.ResponseWriter, r *http.Request) {
	if s.health == nil {
		writeError(w, http.StatusServiceUnavailable, "healthchecks are not available")
		return
	}
	kinds := s.health.Kinds()
	// A stable order, cheapest first, so the page fills in top to bottom.
	sort.Slice(kinds, func(i, j int) bool {
		return kindRank(kinds[i]) < kindRank(kinds[j])
	})
	writeJSON(w, http.StatusOK, map[string]any{"kinds": kinds})
}

// kindRank orders the sections: the AWS canary first because it is the one
// that expires, then binaries which are quick, then the slow MCP probe.
func kindRank(kind string) int {
	switch kind {
	case store.KindAWSProfile:
		return 0
	case store.KindBinary:
		return 1
	case store.KindMCPServer:
		return 2
	default:
		return 3
	}
}
