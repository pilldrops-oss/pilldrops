package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/httpx"
)

// handleLoginEventProxy records a login event, directly when the shared store
// is wired and otherwise via one node (never fan out, nodes share one Postgres).
func (s *Server) handleLoginEventProxy(w http.ResponseWriter, r *http.Request) {
	st := s.st.Load()
	if st == nil {
		s.proxyToNode(w, r, "POST", "/v1/admin/login-event")
		return
	}
	var body struct {
		OwnerID string `json:"owner_id"`
		UserID  string `json:"user_id"`
		Email   string `json:"email"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || body.OwnerID == "" {
		writeErr(w, 400, "owner_id required")
		return
	}
	now := time.Now().Unix()
	if err := st.UpsertLogin(r.Context(), now, body.OwnerID, body.UserID, body.Email); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	_ = st.RecordLoginEvent(r.Context(), now, body.OwnerID, body.Email)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleAdminUsersProxy serves the god-view user list.
func (s *Server) handleAdminUsersProxy(w http.ResponseWriter, r *http.Request) {
	st := s.st.Load()
	if st == nil {
		s.proxyToNode(w, r, "GET", "/v1/admin/users")
		return
	}
	limit := httpx.QueryInt(r, "limit", 200)
	users, err := st.ListUsers(r.Context(), limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"users": users, "count": len(users)})
}

// handleAdminUserDetailProxy serves a single-owner drill-down.
func (s *Server) handleAdminUserDetailProxy(w http.ResponseWriter, r *http.Request) {
	st := s.st.Load()
	if st == nil {
		s.proxyToNode(w, r, "GET", "/v1/admin/users/"+url.PathEscape(r.PathValue("ownerID")))
		return
	}
	d, err := st.GetUserDetail(r.Context(), r.PathValue("ownerID"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if d == nil {
		writeErr(w, 404, "unknown user")
		return
	}
	writeJSON(w, 200, d)
}

// handleAdminOverviewProxy serves the fleet-wide analytics snapshot.
func (s *Server) handleAdminOverviewProxy(w http.ResponseWriter, r *http.Request) {
	st := s.st.Load()
	if st == nil {
		s.proxyToNode(w, r, "GET", "/v1/admin/overview")
		return
	}
	days := httpx.QueryInt(r, "days", 14)
	ov, err := st.AdminOverview(r.Context(), time.Now().Unix(), days)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, ov)
}

// handleAdminSessionsProxy serves the fleet-wide session/login view.
func (s *Server) handleAdminSessionsProxy(w http.ResponseWriter, r *http.Request) {
	st := s.st.Load()
	if st == nil {
		s.proxyToNode(w, r, "GET", "/v1/admin/sessions")
		return
	}
	days := httpx.QueryInt(r, "days", 30)
	a, err := st.AdminSessions(r.Context(), time.Now().Unix(), days)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, a)
}

// handleAdminErrorsProxy lists recent crashed/failed sandboxes fleet-wide.
func (s *Server) handleAdminErrorsProxy(w http.ResponseWriter, r *http.Request) {
	st := s.st.Load()
	if st == nil {
		s.proxyToNode(w, r, "GET", "/v1/admin/errors")
		return
	}
	limit := httpx.QueryInt(r, "limit", 200)
	errs, err := st.AdminErrors(r.Context(), limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"errors": errs, "count": len(errs)})
}

// handleAdminHistoryProxy returns one retained sandbox record with logs, cross-org.
func (s *Server) handleAdminHistoryProxy(w http.ResponseWriter, r *http.Request) {
	st := s.st.Load()
	if st == nil {
		s.proxyToNode(w, r, "GET", "/v1/admin/history/"+url.PathEscape(r.PathValue("id")))
		return
	}
	rec, ok, err := st.GetHistory(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if !ok {
		writeErr(w, 404, "unknown sandbox")
		return
	}
	writeJSON(w, 200, rec)
}

// handleAdminSandboxProxy serves the god-view sandbox drill-down. Only the
// owning node can read the live app log; if it is unreachable, any healthy
// node still answers from the shared store, minus live logs.
func (s *Server) handleAdminSandboxProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := "/v1/admin/sandboxes/" + url.PathEscape(id)
	if node, ok := s.reg.NodeFor(id); ok {
		// forward does not append the query; proxyToNode does, so only add it here.
		nodePath := path
		if r.URL.RawQuery != "" {
			nodePath += "?" + r.URL.RawQuery
		}
		if resp, err := s.forward(r.Context(), node, "GET", nodePath, nil, "", ""); err == nil {
			defer resp.Body.Close()
			relay(w, resp)
			return
		}
	}
	s.proxyToNode(w, r, "GET", path)
}

// handleAdminWhoami is an authorization probe for the admin SPA; s.auth has already passed.
func (s *Server) handleAdminWhoami(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true})
}
