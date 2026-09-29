package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/httpx"
)

// handleLoginEvent upserts a user login. Failures never block sign-in.
func (s *Server) handleLoginEvent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OwnerID string `json:"owner_id"`
		UserID  string `json:"user_id"`
		Email   string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.OwnerID == "" {
		writeErr(w, http.StatusBadRequest, "owner_id required")
		return
	}
	now := time.Now().Unix()
	if err := s.st.UpsertLogin(r.Context(), now, body.OwnerID, body.UserID, body.Email); err != nil {
		writeErr(w, http.StatusInternalServerError, "record login: "+err.Error())
		return
	}
	_ = s.st.RecordLoginEvent(r.Context(), now, body.OwnerID, body.Email)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAdminUsers lists all users with aggregates.
func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	if s.refuseFleetWideTenantData(w) {
		return
	}
	limit := httpx.QueryInt(r, "limit", 200)
	users, err := s.st.ListUsers(r.Context(), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users, "count": len(users)})
}

// handleAdminUserDetail returns the drill-down for one owner.
func (s *Server) handleAdminUserDetail(w http.ResponseWriter, r *http.Request) {
	if s.refuseFleetWideTenantData(w) {
		return
	}
	owner := r.PathValue("ownerID")
	d, err := s.st.GetUserDetail(r.Context(), owner)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if d == nil {
		writeErr(w, http.StatusNotFound, "unknown user")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// handleAdminOverview returns the fleet-wide analytics snapshot.
func (s *Server) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	if s.refuseFleetWideTenantData(w) {
		return
	}
	days := httpx.QueryInt(r, "days", 14)
	ov, err := s.st.AdminOverview(r.Context(), time.Now().Unix(), days)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ov)
}

// handleAdminSessions returns the fleet-wide session view.
func (s *Server) handleAdminSessions(w http.ResponseWriter, r *http.Request) {
	if s.refuseFleetWideTenantData(w) {
		return
	}
	days := httpx.QueryInt(r, "days", 30)
	a, err := s.st.AdminSessions(r.Context(), time.Now().Unix(), days)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// handleAdminErrors lists recent failed sandboxes fleet-wide.
func (s *Server) handleAdminErrors(w http.ResponseWriter, r *http.Request) {
	if s.refuseFleetWideTenantData(w) {
		return
	}
	limit := httpx.QueryInt(r, "limit", 200)
	errs, err := s.st.AdminErrors(r.Context(), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"errors": errs, "count": len(errs)})
}

// handleAdminHistory returns one retained sandbox record with its logs, cross-org.
func (s *Server) handleAdminHistory(w http.ResponseWriter, r *http.Request) {
	if s.refuseFleetWideTenantData(w) {
		return
	}
	rec, ok, err := s.st.GetHistory(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown sandbox")
		return
	}
	writeJSON(w, http.StatusOK, rec)
}
