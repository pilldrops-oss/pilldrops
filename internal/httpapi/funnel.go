package httpapi

import (
	"net/http"
	"strconv"
	"time"
)

// handleAdminFunnel serves the activation funnel and signups by source.
//
//	GET /v1/admin/funnel?days=21
func (s *Server) handleAdminFunnel(w http.ResponseWriter, r *http.Request) {
	if s.refuseFleetWideTenantData(w) {
		return
	}
	days := 21
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
			days = n
		}
	}
	since := time.Now().AddDate(0, 0, -days).Unix()

	f, err := s.st.Funnel(r.Context(), since)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "funnel: "+err.Error())
		return
	}
	bySource, err := s.st.SignupsBySource(r.Context(), since)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "sources: "+err.Error())
		return
	}
	// Rates computed server-side so every dashboard agrees.
	rate := func(n int) float64 {
		if f.Signups == 0 {
			return 0
		}
		return float64(n) / float64(f.Signups) * 100
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"days":              days,
		"signups":           f.Signups,
		"created":           f.Created,
		"alive_at_24h":      f.AliveAt24h,
		"created_pct":       round4(rate(f.Created)),
		"alive_at_24h_pct":  round4(rate(f.AliveAt24h)),
		"signups_by_source": bySource,
	})
}
