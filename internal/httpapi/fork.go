package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
)

// forkReq is what a fork may change; everything else is the parent's.
type forkReq struct {
	Name string `json:"name"`
	// Timeout in seconds; 0 = node default, <0 = persistent.
	Timeout int `json:"timeout"`
	// Visibility of the child: "private" (the default for a fork, even of an
	// org-visible parent) or "org" to hand the copy to the whole org.
	Visibility string `json:"visibility"`
}

// handleFork copies a sandbox into a new one on this node. Gated and counted
// like create: it is a new machine against the org's plan.
func (s *Server) handleFork(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	org := orgID(r)
	guard, done := s.beginIdempotent(r.Context(), w, org, scopedIdemKey("fork", id, r.Header.Get("Idempotency-Key")), http.StatusCreated)
	if done {
		return
	}
	defer guard.release()
	if !s.isolationOK.Load() {
		writeErr(w, http.StatusServiceUnavailable, "node isolation preflight failed — refusing to fork sandboxes")
		return
	}
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	var body forkReq
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
	}
	name, err := sanitizeSandboxName(body.Name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// "" is not "org" here: an unasked-for fork is private to the forker.
	visibility := body.Visibility
	if visibility != "" {
		if visibility, err = sandbox.ValidVisibility(visibility); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	plan := s.orgPlan(r.Context(), org)
	capRelease, ok := s.d.ReserveConcurrency(org, plan.MaxConcurrent)
	if !ok {
		w.Header().Set("Retry-After", "5")
		writeErr(w, http.StatusTooManyRequests,
			fmt.Sprintf("plan '%s' allows %d concurrent sandboxes — pause one or upgrade", plan.ID, plan.MaxConcurrent))
		return
	}
	defer capRelease()
	if s.maxActive > 0 && len(s.d.List("")) >= s.maxActive {
		w.Header().Set("Retry-After", "5")
		writeErr(w, http.StatusTooManyRequests, "sandbox capacity reached")
		return
	}

	res, err := s.d.Fork(r.Context(), id, daemon.ForkSpec{
		Name:    name,
		Timeout: time.Duration(body.Timeout) * time.Second,
		// The child belongs to whoever forked it, not to the parent's creator.
		CreatedBy:  keyIDOf(r),
		Visibility: visibility,
	})
	if err != nil {
		if errors.Is(err, rt.ErrUnsupported) {
			writeErr(w, http.StatusNotImplemented, err.Error())
			return
		}
		writeErr(w, statusForErr(err), err.Error())
		return
	}
	s.metrics.incCreate()
	sb := res.Child
	s.auditDetail(r, "sandbox.fork", sb.ID, map[string]any{
		"parent_id": id, "parent_state": res.ParentState, "template": sb.Template,
		"vcpu": sb.VCPU, "mem_mb": sb.MemMiB, "storage_mb": sb.StorageMB,
		"parent_paused_ms": res.ParentPaused.Milliseconds(), "boot_ms": res.BootMs,
	})
	resp := map[string]any{
		"id": sb.ID, "template": sb.Template, "name": sb.Name, "status": sb.State,
		"created_at": sb.CreatedAt, "expires_at": expiresOrNil(sb.ExpiresAt),
		"created_by": sb.CreatedBy, "visibility": sb.Visibility,
		"terminal_url": s.appURL(r, sb.ID, rt.TerminalPort, org),
		"source":       "fork/" + id, "parent_id": id, "parent_state": res.ParentState,
		"boot_ms": res.BootMs, "parent_paused_ms": res.ParentPaused.Milliseconds(),
	}
	if sb.PublicPort > 0 {
		resp["url"] = s.appURL(r, sb.ID, sb.PublicPort, org)
	}
	guard.finish(resp)
	writeJSON(w, http.StatusCreated, resp)
}
