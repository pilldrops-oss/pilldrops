package orchestrator

import (
	"net/http"
	"strconv"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

// registerPreviewMint mounts the per-port preview token mint. It exists
// because the browser token is port-bound and the ports are not known at create time.
func (s *Server) registerPreviewMint(mux *http.ServeMux) {
	// bySlug like every other /v1/sandboxes/{id} route: this is now the route a
	// client uses to get an openable link for a port it just started something
	// on, and it was the one place a slug stopped working.
	mux.HandleFunc("GET /v1/sandboxes/{id}/preview-token", s.auth(s.bySlug(s.handlePreviewMint)))
}

// handlePreviewMint issues a browser preview token for one port. The caller
// must hold an API credential for the owning org; a preview token does not
// qualify, or a :3000 token could mint one for :8080.
func (s *Server) handlePreviewMint(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	port, err := strconv.Atoi(r.URL.Query().Get("port"))
	if err != nil || port < 1 || port > 65535 {
		writeErr(w, http.StatusBadRequest, `"port" must be 1-65535`)
		return
	}

	if !s.callerOwnsSandbox(r, id) {
		// 404, not 403: a 403 would confirm the id exists for someone else.
		writeErr(w, http.StatusNotFound, "not found")
		return
	}

	tok := s.previewToken(r.Context(), id, port)
	if tok == "" {
		writeErr(w, http.StatusServiceUnavailable, "preview tokens unavailable: no API key configured")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token": tok,
		"port":  port,
	})
}

// callerOwnsSandbox reports whether the request may act on the sandbox: an
// operator always may, a customer only for their org. With no store there is
// no attribution, so a placed sandbox counts as the caller's.
func (s *Server) callerOwnsSandbox(r *http.Request, id string) bool {
	if _, ok := s.reg.NodeFor(id); !ok {
		return false
	}
	keyID, keyRole := callerKey(r.Context())
	// An operator credential acting as itself, with no key named behind it:
	// the fleet console, which sees everything. When a key *is* named — the
	// portal relaying a member — the rule below applies to it like any other.
	if isOperator(r) && keyID == "" {
		return true
	}
	st := s.st.Load()
	if st == nil {
		return true
	}
	// Same lookup as nodePreviewToken, so both hops agree on ownership.
	org, ok := s.previewOrgOf(r.Context(), id)
	if !ok || org == "" {
		// Fail closed: a store blip must not hand one tenant another's token.
		return false
	}
	if org != fastOrg(r) {
		return false
	}
	// A preview token is a way into the sandbox, so it follows the same
	// visibility rule as the API: a member cannot mint one for someone else's
	// private machine. A row we cannot read is refused rather than allowed.
	row, found, err := st.GetActiveSandboxRow(r.Context(), id)
	if err != nil || !found {
		return false
	}
	return sandbox.Visible(row.ID, row.Visibility, row.CreatedBy, keyID, keyRole == "operator")
}
