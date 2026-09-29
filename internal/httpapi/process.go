package httpapi

import (
	"net/http"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

func (s *Server) handleProcList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	procs, err := s.d.Procs(id)
	if err != nil {
		writeErr(w, statusForErr(err), err.Error())
		return
	}
	if procs == nil {
		procs = []rt.Proc{}
	}
	writeJSON(w, http.StatusOK, procs)
}

func (s *Server) handleProcSignal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	var body struct {
		PID    int    `json:"pid"`
		Signal string `json:"signal"`
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	sig := signalNumber(body.Signal)
	if err := s.d.Signal(id, body.PID, sig); err != nil {
		writeErr(w, statusForErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// signalNumber maps signal names to numbers; unknown names default to SIGTERM.
func signalNumber(name string) int {
	switch name {
	case "SIGKILL", "KILL", "9":
		return 9
	case "SIGINT", "INT", "2":
		return 2
	case "SIGHUP", "HUP", "1":
		return 1
	default:
		return 15 // SIGTERM
	}
}
