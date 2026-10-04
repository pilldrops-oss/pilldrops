package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
)

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	org := orgID(r)
	guard, done := s.beginIdempotent(r.Context(), w, org, scopedIdemKey("pause", id, r.Header.Get("Idempotency-Key")), http.StatusOK)
	if done {
		return
	}
	defer guard.release()
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	took, err := s.d.PauseTimed(r.Context(), id)
	if err != nil {
		writeErr(w, statusForErr(err), err.Error())
		return
	}
	s.auditDetail(r, "sandbox.pause", id, map[string]any{"paused_ms": took.Milliseconds()})
	resp := map[string]any{"status": "paused", "paused_ms": took.Milliseconds()}
	guard.finish(resp)
	writeJSON(w, http.StatusOK, resp)
}

// handleStandby freezes a running sandbox in RAM. Never gated, like pause:
// stopping your own machine's compute must always be allowed.
func (s *Server) handleStandby(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	org := orgID(r)
	guard, done := s.beginIdempotent(r.Context(), w, org, scopedIdemKey("standby", id, r.Header.Get("Idempotency-Key")), http.StatusOK)
	if done {
		return
	}
	defer guard.release()
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	took, err := s.d.Standby(r.Context(), id)
	if err != nil {
		if errors.Is(err, rt.ErrUnsupported) {
			writeErr(w, http.StatusNotImplemented, err.Error())
			return
		}
		writeErr(w, statusForErr(err), err.Error())
		return
	}
	s.auditDetail(r, "sandbox.standby", id, map[string]any{"standby_ms": took.Milliseconds()})
	resp := map[string]any{"status": "standby", "standby_ms": took.Milliseconds()}
	guard.finish(resp)
	writeJSON(w, http.StatusOK, resp)
}

// handleResume restarts a paused sandbox. Resume boots like create and is
// gated like it; pause is never gated, since stopping your own machine must
// always be allowed.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	org := orgID(r)
	guard, done := s.beginIdempotent(r.Context(), w, org, scopedIdemKey("resume", id, r.Header.Get("Idempotency-Key")), http.StatusOK)
	if done {
		return
	}
	defer guard.release()
	if !s.isolationOK.Load() {
		writeErr(w, http.StatusServiceUnavailable, "node isolation preflight failed — refusing to resume sandboxes")
		return
	}
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	took, err := s.d.ResumeTimed(r.Context(), id)
	if err != nil {
		writeErr(w, statusForErr(err), err.Error())
		return
	}
	s.auditDetail(r, "sandbox.resume", id, map[string]any{"resume_ms": took.Milliseconds()})
	resp := map[string]any{"status": "running", "resume_ms": took.Milliseconds()}
	guard.finish(resp)
	writeJSON(w, http.StatusOK, resp)
}

// handleTimeout re-stamps a sandbox's hard TTL under the same plan rules as
// create, since it is how a leased sandbox becomes a workstation.
func (s *Server) handleTimeout(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	var body struct {
		Seconds *int `json:"seconds"`
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// A TTL of zero expires the sandbox on the spot, which is what a client
	// sending the wrong field name used to get.
	if body.Seconds == nil || *body.Seconds == 0 {
		writeErr(w, http.StatusBadRequest, `"seconds" is required: a positive TTL from now, or -1 for no expiry`)
		return
	}
	seconds := *body.Seconds
	org := orgID(r)
	plan := s.orgPlan(r.Context(), org)
	ttl := time.Duration(seconds) * time.Second
	if seconds < 0 {
		if !plan.Persistent {
			writeErr(w, http.StatusForbidden, fmt.Sprintf(
				"plan '%s' includes no persistent workstations — upgrade to keep a sandbox with no expiry",
				plan.ID))
			return
		}
		// This sandbox is excluded from the tally; re-asserting persistence on
		// it is not a request for one more.
		if n := persistentCount(othersThan(s.d.List(org), id)); plan.MaxWorkstations > 0 && n >= plan.MaxWorkstations {
			writeErr(w, http.StatusTooManyRequests, fmt.Sprintf(
				"plan '%s' includes %d persistent workstation(s) — delete one or upgrade",
				plan.ID, plan.MaxWorkstations))
			return
		}
		ttl = -1
	}
	planTTL := time.Duration(plan.SessionTTLHrs) * time.Hour
	if ttl > 0 && planTTL > 0 && ttl > planTTL {
		ttl = planTTL
	}
	if err := s.d.SetTimeout(r.Context(), id, ttl); err != nil {
		writeErr(w, statusForErr(err), err.Error())
		return
	}
	// Audit the granted timeout alongside the request so a clamp is visible.
	s.auditDetail(r, "sandbox.timeout", id, map[string]any{
		"timeout_secs":   int(ttl / time.Second),
		"requested_secs": body.Seconds,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// othersThan drops one sandbox from a list.
func othersThan(boxes []daemon.SandboxInfo, id string) []daemon.SandboxInfo {
	out := make([]daemon.SandboxInfo, 0, len(boxes))
	for _, sb := range boxes {
		if sb.ID != id {
			out = append(out, sb)
		}
	}
	return out
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	if err := s.d.Refresh(r.Context(), id); err != nil {
		writeErr(w, statusForErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleRehome takes over a paused sandbox whose node is gone, from its
// archived pause snapshot. The control plane's call alone: it decides a node is
// gone, and moving a record between nodes is not a tenant's to ask for.
func (s *Server) handleRehome(w http.ResponseWriter, r *http.Request) {
	if roleOf(r) != "operator" {
		writeErr(w, http.StatusForbidden, "only the control plane may rehome a sandbox")
		return
	}
	id := r.PathValue("id")
	err := s.d.Rehome(r.Context(), id, orgID(r))
	switch {
	case err == nil:
		s.audit(r, "sandbox.rehome", id)
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "state": "running"})
	case errors.Is(err, rt.ErrCPUTooPoor):
		nodeBusy(w) // another node's CPU may run it
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, daemon.ErrNotRehomable):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		writeErr(w, statusForErr(err), err.Error())
	}
}
