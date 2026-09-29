package orchestrator

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/httpx"
)

// handleWaitlistCreateProxy records a "notify me" signup from the portal, which
// validates and rate-limits the visitor itself. Falls back to a node without a store.
func (s *Server) handleWaitlistCreateProxy(w http.ResponseWriter, r *http.Request) {
	st := s.st.Load()
	if st == nil {
		s.proxyToNode(w, r, "POST", "/v1/waitlist")
		return
	}
	var body struct {
		Email  string `json:"email"`
		Source string `json:"source"`
		Note   string `json:"note"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeErr(w, 400, "bad request")
		return
	}
	// AddWaitlist validates too, but its error would read as a store failure.
	email := strings.TrimSpace(body.Email)
	if email == "" || len(email) > 254 || !strings.Contains(email, "@") {
		writeErr(w, 400, "a valid email is required")
		return
	}
	if err := st.AddWaitlist(r.Context(), body.Email, body.Source, body.Note, time.Now().Unix()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleAdminWaitlistProxy lists waitlist signups newest-first for the admin console.
func (s *Server) handleAdminWaitlistProxy(w http.ResponseWriter, r *http.Request) {
	st := s.st.Load()
	if st == nil {
		s.proxyToNode(w, r, "GET", "/v1/admin/waitlist")
		return
	}
	limit := httpx.QueryInt(r, "limit", 200)
	rows, err := st.ListWaitlist(r.Context(), limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"waitlist": rows, "count": len(rows)})
}
