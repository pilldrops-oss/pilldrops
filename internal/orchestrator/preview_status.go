package orchestrator

import "net/http"

// handlePreviewStatus relays the node's probe. The node owns the answer: it
// knows the published port, reports "terminal"/"unpublished"/"paused", and
// never resumes a paused sandbox to answer — which probing through the
// /preview proxy from here did.
func (s *Server) handlePreviewStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	node, ok := s.reg.NodeFor(id)
	// Same rule as preview-token: this reports whether the sandbox's own app is
	// serving, so a member may not point it at someone else's private machine.
	// 404, not 403 — a 403 confirms the id exists for someone else.
	if !ok || !s.callerOwnsSandbox(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	path := "/v1/sandboxes/" + id + "/preview-status"
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	resp, err := s.forward(r.Context(), node, "GET", path, nil, "", fastOrg(r))
	if err != nil {
		writeErr(w, 502, "node request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	relay(w, resp)
}
