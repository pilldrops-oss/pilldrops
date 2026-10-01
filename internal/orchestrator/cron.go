package orchestrator

import (
	"encoding/json"
	"io"
	"net/http"
)

// Creating a job and running it now both act on the job's sandbox, so they go
// to the node that hosts it; any other node answers 404. The rest of /v1/cron
// reads and writes the shared table, which the catch-all serves from any node.
func (s *Server) registerCronRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/cron", s.auth(s.handleCronCreateProxy))
	mux.HandleFunc("POST /v1/cron/{id}/run", s.auth(s.handleCronRunProxy))
}

func (s *Server) handleCronCreateProxy(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read the request body")
		return
	}
	var job struct {
		SandboxID string `json:"sandbox_id"`
	}
	if json.Unmarshal(body, &job) != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	s.forwardToSandboxNode(w, r, job.SandboxID, body)
}

func (s *Server) handleCronRunProxy(w http.ResponseWriter, r *http.Request) {
	st := s.st.Load()
	if st == nil {
		s.proxyToNode(w, r, r.Method, r.URL.Path)
		return
	}
	job, ok := st.CronJobByID(r.Context(), fastOrg(r), r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	s.forwardToSandboxNode(w, r, job.SandboxID, nil)
}

// forwardToSandboxNode relays r, with body, to the node that hosts sandboxID.
func (s *Server) forwardToSandboxNode(w http.ResponseWriter, r *http.Request, sandboxID string, body []byte) {
	node, ok := s.reg.NodeFor(sandboxID)
	if !ok {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	resp, err := s.forward(r.Context(), node, r.Method, r.URL.Path, body, r.Header.Get("Content-Type"), fastOrg(r))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "node request failed: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort: relayed to the client below
	relay(w, resp)
}
