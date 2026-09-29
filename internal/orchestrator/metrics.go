package orchestrator

import "net/http"

func (s *Server) handleSandboxMetrics(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	node, ok := s.reg.NodeFor(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	resp, err := s.forward(r.Context(), node, "GET", "/v1/sandboxes/"+id+"/metrics", nil, "", fastOrg(r))
	if err != nil {
		writeErr(w, 502, "node request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	relay(w, resp)
}
