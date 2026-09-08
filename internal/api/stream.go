package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/raulsh/claude-scheduler/internal/executor"
	"github.com/raulsh/claude-scheduler/internal/store"
)

// sseHeartbeat keeps idle connections alive through proxies and lets the
// client notice a dead server.
const sseHeartbeat = 20 * time.Second

// handleStreamExecution streams an execution's transcript as Server-Sent
// Events. Clients resume with Last-Event-ID (or ?after_seq=), and the
// response backfills from the database before attaching to the live feed, so
// a reconnect never loses events.
func (s *Server) handleStreamExecution(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid execution id")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	exec, err := s.store.GetExecution(r.Context(), id)
	if err != nil {
		writeStoreError(w, err, "get execution")
		return
	}

	afterSeq := resumeFrom(r)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Defeat proxy buffering, which would otherwise defer every event.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Attach to the live feed first, so events published while the backlog is
	// being written are queued rather than lost.
	var sub *executor.Subscription
	finished := store.Terminal(exec.Status)
	if s.executor != nil {
		var alreadyClosed bool
		sub, alreadyClosed = s.executor.Broker().Subscribe(id, afterSeq)
		defer sub.Close()
		finished = finished || alreadyClosed
	}

	// Backfill everything already persisted.
	lastSeq := afterSeq
	for {
		events, err := s.store.ListEvents(r.Context(), id, lastSeq, 500)
		if err != nil {
			s.log.Warn("could not backfill stream", "execution", id, "error", err)
			break
		}
		if len(events) == 0 {
			break
		}
		for _, ev := range events {
			writeSSE(w, flusher, ev.Seq, executor.Message{
				Seq:     ev.Seq,
				Type:    ev.Type,
				Subtype: ev.Subtype,
				Payload: json.RawMessage(ev.Payload),
			})
			lastSeq = ev.Seq
		}
	}

	// A finished execution needs no live feed; close after a terminal frame.
	if finished {
		writeSSE(w, flusher, 0, executor.Message{
			Type: "status", Status: exec.Status, Terminal: true,
		})
		return
	}

	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case <-heartbeat.C:
			// A comment frame: valid SSE that clients ignore.
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()

		case msg, open := <-sub.C:
			if !open {
				return
			}
			// The backlog may overlap what was already backfilled.
			if msg.Seq != 0 && msg.Seq <= lastSeq {
				continue
			}
			if msg.Seq > lastSeq {
				lastSeq = msg.Seq
			}
			writeSSE(w, flusher, msg.Seq, msg)
			if msg.Terminal {
				return
			}
		}
	}
}

// resumeFrom reads the resume point from Last-Event-ID, falling back to the
// after_seq query parameter.
func resumeFrom(r *http.Request) int64 {
	if raw := r.Header.Get("Last-Event-ID"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	if raw := r.URL.Query().Get("after_seq"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return 0
}

// writeSSE emits one event frame. Only sequenced events carry an id, so a
// reconnect resumes from a real transcript position rather than a status
// frame.
func writeSSE(w http.ResponseWriter, flusher http.Flusher, seq int64, msg executor.Message) {
	payload, err := json.Marshal(msg)
	if err != nil {
		return
	}
	if seq > 0 {
		fmt.Fprintf(w, "id: %d\n", seq)
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", sseEventName(msg), payload)
	flusher.Flush()
}

func sseEventName(msg executor.Message) string {
	if msg.Type == "" {
		return "message"
	}
	return msg.Type
}
