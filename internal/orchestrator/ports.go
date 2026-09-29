package orchestrator

import "net/http"

func (s *Server) handleSandboxPorts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	node, ok := s.reg.NodeFor(id)
	// Placement alone is not permission: a port listing enumerates what a
	// sandbox is serving, so it takes the same ownership rule as every other
	// read of it. 404, not 403 — a 403 confirms the id exists for someone else.
	if !ok || !s.callerOwnsSandbox(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	resp, err := s.forward(r.Context(), node, "GET", "/v1/sandboxes/"+id+"/ports", nil, "", fastOrg(r))
	if err != nil {
		writeErr(w, 502, "node request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	relay(w, resp)
}
