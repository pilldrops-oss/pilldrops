package httpapi

import (
	"errors"
	"net/http"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
)

// registerCheckpointRoutes mounts the named save-point verbs through Register's
// own route wrapper, so they keep its metrics and auth.
func (s *Server) registerCheckpointRoutes(route func(string, http.Handler)) {
	route("POST /v1/sandboxes/{id}/checkpoints", s.auth(s.handleCheckpointSave))
	route("GET /v1/sandboxes/{id}/checkpoints", s.auth(s.handleCheckpointList))
	route("DELETE /v1/sandboxes/{id}/checkpoints/{name}", s.auth(s.handleCheckpointRemove))
	route("POST /v1/sandboxes/{id}/checkpoints/{name}/restore", s.auth(s.handleCheckpointRestore))
}

// checkpointErr maps the daemon's checkpoint refusals to status codes. A runtime
// that cannot checkpoint answers 501 naming itself, as fork does.
func checkpointErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, rt.ErrUnsupported):
		writeErr(w, http.StatusNotImplemented, err.Error())
	case errors.Is(err, daemon.ErrCheckpointLimit):
		writeErrCode(w, http.StatusConflict, "checkpoint_limit", err.Error())
	case errors.Is(err, daemon.ErrCheckpointNotReady):
		writeErrCode(w, http.StatusConflict, "checkpoint_not_ready", err.Error())
	default:
		writeErr(w, statusForErr(err), err.Error())
	}
}

// reservedCheckpoint refuses a name the platform keeps for its own captures.
// A backup's staging, its materialisation and a snapshot export are all
// capture directories beside the owner's save points, and the checkpoint verbs
// must not reach them: removing one mid-save destroys a backup or a snapshot
// that is being written, and restoring one rolls the machine into a capture
// nobody asked for.
func reservedCheckpoint(w http.ResponseWriter, name string) bool {
	if !daemon.InternalCapture(name) {
		return false
	}
	writeErr(w, http.StatusNotFound, "no such checkpoint: "+name)
	return true
}

// handleCheckpointSave takes a named save point of a running sandbox.
func (s *Server) handleCheckpointSave(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Name == "" {
		writeErr(w, http.StatusBadRequest, "name required")
		return
	}
	if daemon.InternalCapture(body.Name) {
		writeErr(w, http.StatusBadRequest, "checkpoint name "+body.Name+" uses a reserved prefix")
		return
	}
	c, err := s.d.Checkpoint(r.Context(), id, body.Name)
	if err != nil {
		checkpointErr(w, err)
		return
	}
	s.auditDetail(r, "sandbox.checkpoint.save", id, map[string]any{
		"name": c.Name, "kind": c.Kind, "size_bytes": c.SizeBytes,
	})
	writeJSON(w, http.StatusCreated, checkpointBody(c))
}

// handleCheckpointList lists a sandbox's save points.
func (s *Server) handleCheckpointList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	list, err := s.d.ListCheckpoints(id)
	if err != nil {
		checkpointErr(w, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, c := range list {
		out = append(out, checkpointBody(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"checkpoints": out, "max": daemon.MaxCheckpoints})
}

// handleCheckpointRemove deletes one save point.
func (s *Server) handleCheckpointRemove(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.PathValue("name")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	if reservedCheckpoint(w, name) {
		return
	}
	if err := s.d.RemoveCheckpoint(id, name); err != nil {
		checkpointErr(w, err)
		return
	}
	s.auditDetail(r, "sandbox.checkpoint.remove", id, map[string]any{"name": name})
	writeJSON(w, http.StatusOK, map[string]any{"status": "removed", "name": name})
}

// handleCheckpointRestore rolls the sandbox back to a save point in place. The
// id, URL, token and address are unchanged, so this is not gated like a create.
func (s *Server) handleCheckpointRestore(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.PathValue("name")
	if !s.isolationOK.Load() {
		writeErr(w, http.StatusServiceUnavailable, "node isolation preflight failed — refusing to restore sandboxes")
		return
	}
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	if reservedCheckpoint(w, name) {
		return
	}
	took, err := s.d.RestoreCheckpoint(r.Context(), id, name)
	if err != nil {
		checkpointErr(w, err)
		return
	}
	s.auditDetail(r, "sandbox.checkpoint.restore", id, map[string]any{
		"name": name, "restore_ms": took.Milliseconds(),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "running", "name": name, "restore_ms": took.Milliseconds(),
	})
}

// checkpointBody renders one checkpoint. snapshot_kind is spelled as fork and
// pause spell it, so a client reads one field name across every capture verb.
//
// paused_ms is how long the guest stood still for the capture, which is the
// whole claim a save point makes ("the machine keeps running") and was measured
// on every capture and then dropped here. Always present, including the 0 an
// engine that captures without stopping the guest reports.
func checkpointBody(c daemon.Checkpoint) map[string]any {
	return map[string]any{
		"name": c.Name, "created_at": c.CreatedAt, "size_bytes": c.SizeBytes,
		"snapshot_kind": c.Kind, "status": c.Status, "paused_ms": c.PausedMs,
	}
}
