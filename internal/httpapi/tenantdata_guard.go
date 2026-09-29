package httpapi

import "net/http"

// refuseFleetWideTenantData 404s fleet-wide tenant reads on a clustered node so
// a node cannot enumerate every customer. Checked per request because
// Clustered() is only known after boot wiring.
func (s *Server) refuseFleetWideTenantData(w http.ResponseWriter) bool {
	if !s.Clustered() {
		return false
	}
	// 404, not 403: the endpoint does not exist as a concept here.
	writeErr(w, http.StatusNotFound,
		"fleet-wide tenant data is served by the control plane, not by a node")
	return true
}
