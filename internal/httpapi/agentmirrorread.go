//go:build !core

package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/extras/agent/agentd"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// Reads that fall back to the control plane's mirror. History is mirror-first
// since it survives the sandbox; liveness is live-first since a stale "idle"
// mid-turn is wrong. Reads never wake a paused sandbox.

// servedFrom labels where a response came from, so a client never infers it from a timestamp.
const (
	servedFromNode   = "node"
	servedFromMirror = "mirror"
)

// agentUnavailableLive reports whether the live agent cannot answer at all,
// so the mirror should. A refusal from the supervisor is a real answer and is
// excluded; unknown errors surface rather than being answered from the mirror.
func agentUnavailableLive(err error) bool {
	if err == nil {
		return false
	}
	var se *agentd.StatusError
	if errors.As(err, &se) {
		return false
	}
	return errors.Is(err, rt.ErrPaused) ||
		errors.Is(err, rt.ErrNotFound) ||
		errors.Is(err, rt.ErrUnsupported) ||
		errors.Is(err, agentd.ErrAgentUnreachable)
}

// mirroredTurnsJSON renders mirror rows in the live endpoint's shape. Fields
// the mirror lacks are omitted, not defaulted.
func mirroredTurnsJSON(turns []store.MirroredTurn) []map[string]any {
	out := make([]map[string]any, 0, len(turns))
	for _, t := range turns {
		m := map[string]any{
			"id": t.ID, "request": t.Request, "state": t.State,
			"queued_at": t.QueuedAt.UTC().Format(time.RFC3339),
		}
		for k, v := range map[string]string{
			"response": t.Response, "mode": t.Mode, "source": t.Source,
			"error": t.Error, "session_id": t.SessionID,
		} {
			if v != "" {
				m[k] = v
			}
		}
		if t.ExitCode != nil {
			m["exit_code"] = *t.ExitCode
		}
		// Absent means unknown; a 0 would make a spend total look complete.
		if t.CostUSD != nil {
			m["cost_usd"] = *t.CostUSD
		}
		if !t.StartedAt.IsZero() {
			m["started_at"] = t.StartedAt.UTC().Format(time.RFC3339)
		}
		if !t.EndedAt.IsZero() {
			m["ended_at"] = t.EndedAt.UTC().Format(time.RFC3339)
		}
		if t.SeqStart > 0 {
			m["seq_start"] = t.SeqStart
		}
		if t.SeqEnd > 0 {
			m["seq_end"] = t.SeqEnd
		}
		out = append(out, m)
	}
	return out
}

// serveMirroredTurns answers a turns read from the mirror; note explains why.
func (s *Server) serveMirroredTurns(w http.ResponseWriter, r *http.Request, id, note string) bool {
	rows, err := s.st.ListMirroredTurns(r.Context(), orgID(r), id, 100)
	if err != nil || len(rows) == 0 {
		// Report the live failure rather than an empty list that claims the agent never ran.
		return false
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"turns":       mirroredTurnsJSON(rows),
		"served_from": servedFromMirror,
		"note":        note,
	})
	return true
}

// serveMirroredStatus answers a status read from the mirror. The state is
// "stopped", since "idle" would claim a supervisor is up.
func (s *Server) serveMirroredStatus(w http.ResponseWriter, r *http.Request, id string, tmpl templates.Template) bool {
	rows, err := s.st.ListMirroredTurns(r.Context(), orgID(r), id, 1)
	if err != nil {
		return false
	}
	cursor, _ := s.st.MirrorCursor(r.Context(), id)
	if len(rows) == 0 && cursor == 0 {
		// Never mirrored; the mirror cannot claim the agent did nothing.
		return false
	}

	session := map[string]any{
		"state":    "stopped",
		"last_seq": cursor,
		"degraded_reason": "this workstation is not running; the control plane is " +
			"reporting the last state it recorded",
	}
	if len(rows) > 0 {
		last := rows[0]
		session["last_turn"] = mirroredTurnsJSON(rows)[0]
		if !last.EndedAt.IsZero() {
			session["last_activity_at"] = last.EndedAt.UTC().Format(time.RFC3339)
		} else if !last.QueuedAt.IsZero() {
			session["last_activity_at"] = last.QueuedAt.UTC().Format(time.RFC3339)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sandbox_id":  id,
		"agent":       map[string]any{"id": tmpl.ID, "name": tmpl.Name},
		"session":     session,
		"served_from": servedFromMirror,
	})
	return true
}

// replayMirroredEvents serves the mirrored timeline as SSE and closes. The
// first frame is a mirror.info event because EventSource hides SSE comments.
// Returns false when the mirror has nothing.
func (s *Server) replayMirroredEvents(w http.ResponseWriter, r *http.Request, flusher http.Flusher, id string, since uint64) bool {
	rows, err := s.st.ListMirroredEvents(r.Context(), orgID(r), id, since, 1000)
	if err != nil {
		return false
	}
	cursor, _ := s.st.MirrorCursor(r.Context(), id)
	if len(rows) == 0 && cursor == 0 {
		// Never mirrored, as opposed to mirrored and already up to date.
		return false
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Slow EventSource's auto-reconnect; nothing changes until the workstation wakes.
	fmt.Fprint(w, "retry: 60000\n\n")
	info, _ := json.Marshal(map[string]any{
		"served_from": servedFromMirror,
		"reason": "this workstation is not running; this is the control plane's copy of " +
			"what it did. It will not update until the workstation is woken.",
		"last_seq": cursor,
	})
	fmt.Fprintf(w, "event: mirror.info\ndata: %s\n\n", info)

	for _, e := range rows {
		// seq is the SSE id, so a reconnect resumes against either source.
		frame, err := json.Marshal(map[string]any{
			"seq": e.Seq, "at": e.At, "type": e.Type,
			"turn_id": e.TurnID, "step": e.Step, "payload": e.Payload,
		})
		if err != nil {
			continue
		}
		if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.Seq, frame); err != nil {
			return true
		}
	}
	flusher.Flush()
	return true
}
