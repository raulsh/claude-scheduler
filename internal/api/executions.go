package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/raulsh/claude-scheduler/internal/store"
)

// handleListExecutions serves the filterable execution list. Every filter is
// a query parameter so the UI's filter state stays URL-addressable.
func (s *Server) handleListExecutions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.ExecutionFilter{
		Query:  q.Get("q"),
		Cursor: q.Get("cursor"),
	}

	if raw := q.Get("task_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "task_id must be an integer")
			return
		}
		filter.TaskID = &id
	}

	// status is repeatable and also accepts a comma-separated list.
	for _, v := range q["status"] {
		for _, st := range strings.Split(v, ",") {
			if st = strings.TrimSpace(st); st != "" {
				filter.Statuses = append(filter.Statuses, st)
			}
		}
	}

	var err error
	if filter.From, err = parseTimeParam(q.Get("from")); err != nil {
		writeError(w, http.StatusBadRequest, "from: "+err.Error())
		return
	}
	if filter.To, err = parseTimeParam(q.Get("to")); err != nil {
		writeError(w, http.StatusBadRequest, "to: "+err.Error())
		return
	}

	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		filter.Limit = n
	}

	execs, next, err := s.store.ListExecutions(r.Context(), filter)
	if err != nil {
		writeStoreError(w, err, "list executions")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"executions":  orEmpty(execs),
		"next_cursor": next,
	})
}

func (s *Server) handleGetExecution(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid execution id")
		return
	}

	exec, err := s.store.GetExecution(r.Context(), id)
	if err != nil {
		writeStoreError(w, err, "get execution")
		return
	}
	writeJSON(w, http.StatusOK, exec)
}

// handleListEvents serves an execution's stored transcript. The UI uses this
// for finished runs and as the backfill before attaching to a live stream.
func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid execution id")
		return
	}

	q := r.URL.Query()
	var afterSeq int64
	if raw := q.Get("after_seq"); raw != "" {
		if afterSeq, err = strconv.ParseInt(raw, 10, 64); err != nil {
			writeError(w, http.StatusBadRequest, "after_seq must be an integer")
			return
		}
	}
	limit := 0
	if raw := q.Get("limit"); raw != "" {
		if limit, err = strconv.Atoi(raw); err != nil {
			writeError(w, http.StatusBadRequest, "limit must be an integer")
			return
		}
	}

	events, err := s.store.ListEvents(r.Context(), id, afterSeq, limit)
	if err != nil {
		writeStoreError(w, err, "list events")
		return
	}

	// Payloads are already JSON, so they are relayed verbatim rather than
	// re-encoded into a string.
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		out = append(out, map[string]any{
			"seq":     e.Seq,
			"ts":      e.TS,
			"type":    e.Type,
			"subtype": e.Subtype,
			"payload": rawJSON(e.Payload),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

// rawJSON relays already-encoded JSON without a second round of escaping.
type rawJSON []byte

func (r rawJSON) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	return r, nil
}

// parseTimeParam accepts RFC3339, or a bare date for day-granularity filters.
func parseTimeParam(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errInvalidTime
}

var errInvalidTime = errors.New("expected an RFC3339 timestamp or YYYY-MM-DD date")

// orEmpty keeps JSON arrays as [] rather than null, which spares every
// caller a nil check.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
