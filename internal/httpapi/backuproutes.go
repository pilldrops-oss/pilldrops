package httpapi

import (
	"errors"
	"net/http"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// registerBackupRoutes mounts the backup verbs through Register's own route
// wrapper, so they keep its metrics and auth. The orchestrator's sandbox
// catch-all carries them to the owning node unchanged.
func (s *Server) registerBackupRoutes(route func(string, http.Handler)) {
	route("POST /v1/sandboxes/{id}/backups", s.auth(s.handleBackupCreate))
	route("GET /v1/sandboxes/{id}/backups", s.auth(s.handleBackupList))
	route("DELETE /v1/sandboxes/{id}/backups/{bkp}", s.auth(s.handleBackupDelete))
	route("POST /v1/sandboxes/{id}/backups/{bkp}/restore", s.auth(s.handleBackupRestore))
}

// backupErr maps the daemon's refusals to status codes. A node with no object
// store, and a runtime that cannot capture, both answer 501 naming what is
// missing rather than failing somewhere deeper with a confusing message.
func backupErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, daemon.ErrBackupsUnconfigured):
		writeErrCode(w, http.StatusNotImplemented, "backups_need_object_store", err.Error())
	case errors.Is(err, rt.ErrUnsupported):
		writeErr(w, http.StatusNotImplemented, err.Error())
	case errors.Is(err, daemon.ErrBackupInFlight):
		writeErrCode(w, http.StatusConflict, "backup_in_flight", err.Error())
	case errors.Is(err, store.ErrBackupReferenced):
		writeErrCode(w, http.StatusConflict, "backup_referenced", err.Error())
	case errors.Is(err, daemon.ErrBackupNotReady):
		writeErrCode(w, http.StatusConflict, "backup_not_ready", err.Error())
	case errors.Is(err, daemon.ErrRestoreNotConfirmed):
		writeErrCode(w, http.StatusBadRequest, "confirm_required", err.Error())
	default:
		writeErr(w, statusForErr(err), err.Error())
	}
}

// handleBackupCreate captures the sandbox and starts the upload. It returns as
// soon as the sandbox is running again, so the body is `pending` and not the
// finished backup.
func (s *Server) handleBackupCreate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	started, err := s.d.Backup(r.Context(), id)
	if err != nil {
		backupErr(w, err)
		return
	}
	s.auditDetail(r, "sandbox.backup.create", id, map[string]any{
		"backup": started.ID, "kind": started.Kind, "paused_ms": started.PausedMs,
	})
	body := backupBody(started.Backup)
	// How long the machine stood still, which is the cost an operator is
	// deciding about; the row cannot carry it, since it is a fact about this
	// capture rather than about the backup it produced.
	body["paused_ms"] = started.PausedMs
	writeJSON(w, http.StatusAccepted, body)
}

// handleBackupList lists a sandbox's backups, newest first.
func (s *Server) handleBackupList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	list, err := s.d.ListBackups(r.Context(), orgID(r), id)
	if err != nil {
		backupErr(w, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, b := range list {
		out = append(out, backupBody(b))
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": out})
}

// handleBackupDelete removes one backup's object and row.
func (s *Server) handleBackupDelete(w http.ResponseWriter, r *http.Request) {
	id, bkp := r.PathValue("id"), r.PathValue("bkp")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	if err := s.d.DeleteBackup(r.Context(), orgID(r), id, bkp); err != nil {
		backupErr(w, err)
		return
	}
	s.auditDetail(r, "sandbox.backup.delete", id, map[string]any{"backup": bkp})
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "id": bkp})
}

// handleBackupRestore overwrites the sandbox's disk with a backup's. This is
// the one verb here that destroys data the caller still has, so an unconfirmed
// request changes nothing and says why.
func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	id, bkp := r.PathValue("id"), r.PathValue("bkp")
	if !s.isolationOK.Load() {
		writeErr(w, http.StatusServiceUnavailable, "node isolation preflight failed — refusing to restore sandboxes")
		return
	}
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	var body struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	took, err := s.d.RestoreBackup(r.Context(), orgID(r), id, bkp, body.Confirm)
	if err != nil {
		backupErr(w, err)
		return
	}
	s.auditDetail(r, "sandbox.backup.restore", id, map[string]any{
		"backup": bkp, "restore_ms": took.Milliseconds(),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "running", "id": bkp, "restore_ms": took.Milliseconds(),
	})
}

// backupBody renders one backup. snapshot_kind is spelled as checkpoint, fork
// and pause spell it, and it carries what this capture actually holds —
// "memory" on an engine that captured RAM, "disk" on one that captured the
// filesystem — rather than a constant that would claim a memory image exists
// on a runtime that has none.
func backupBody(b store.Backup) map[string]any {
	out := map[string]any{
		"id": b.ID, "status": b.Status, "bytes": b.Bytes,
		"snapshot_kind": b.Kind, "created_at": b.CreatedAt,
	}
	if b.ReadyAt != 0 {
		out["ready_at"] = b.ReadyAt
	}
	if b.ExpiresAt != 0 {
		out["expires_at"] = b.ExpiresAt
	}
	if b.SHA256 != "" {
		out["sha256"] = b.SHA256
	}
	// A deduped backup holds no bytes of its own; saying so is the difference
	// between "this is tiny" and "this is a pointer at the one before it".
	if b.ParentID != "" {
		out["parent_id"] = b.ParentID
		out["deduped"] = true
	}
	if b.Error != "" {
		out["error"] = b.Error
	}
	return out
}
