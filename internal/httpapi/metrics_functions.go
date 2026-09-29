//go:build !core

package httpapi

import "net/http"

// handleFuncMetrics returns registry counters plus live usage from the function's
// sandbox, or zeros and state "cold" when it has none.
func (s *Server) handleFuncMetrics(w http.ResponseWriter, r *http.Request) {
	fn, ok := s.lookupFunc(w, r)
	if !ok {
		return
	}
	resp := map[string]any{
		"invokes":         fn.Invokes,
		"cold_starts":     fn.ColdStarts,
		"last_invoked_at": fn.LastInvokedAt,
		"state":           "cold",
		"vcpu_seconds":    0.0,
		"ram_gb_seconds":  0.0,
		"uptime_seconds":  0.0,
	}
	if fn.SandboxID != "" {
		if u, live := s.d.Usage(fn.SandboxID); live {
			resp["state"] = string(u.State)
			resp["vcpu_seconds"] = u.VCPUSeconds
			resp["ram_gb_seconds"] = u.RAMGBSeconds
			resp["uptime_seconds"] = u.UptimeSeconds
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
