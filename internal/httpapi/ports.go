package httpapi

import (
	"net/http"
	"slices"
)

// candidatePorts are probed because the runtime interface cannot enumerate sockets; best effort only.
var candidatePorts = []int{3000, 3001, 4200, 5000, 5173, 8000, 8080, 8081, 8888, 9000, 9323}

// handleSandboxPorts reports which of candidatePorts, plus the sandbox's declared port, answer.
func (s *Server) handleSandboxPorts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sb, ok := s.d.Get(id)
	// owns, not an org comparison: the org is only half the rule, and a private
	// sandbox must not be enumerable by every key in its org.
	if !ok || !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}

	candidates := candidatePorts
	if sb.PublicPort > 0 && !slices.Contains(candidates, sb.PublicPort) {
		candidates = append(append([]int{}, candidates...), sb.PublicPort)
	}

	ports, err := s.d.DetectPorts(r.Context(), id, candidates)
	if err != nil {
		writeErr(w, statusForErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ports": ports})
}
