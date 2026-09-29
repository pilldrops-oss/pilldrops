package dashboard

import (
	"encoding/json"
	"net/http"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) statsSnapshot() map[string]any {
	boxes := s.d.List("")
	perTemplate := map[string]int{}
	for _, b := range boxes {
		perTemplate[b.Template]++
	}
	active := map[string]any{}
	for _, t := range s.templates {
		active[t] = map[string]any{"template": t, "active": perTemplate[t]}
	}
	return map[string]any{
		"active_count": len(boxes), "max_active": s.limits.MaxActive,
		"disk_usage": 0.0, "disk_pressure": false,
		"template_stats": active,
		"rate_limit_rps": s.limits.RateRPS, "rate_limit_burst": s.limits.RateBurst,
		"max_concurrent": s.limits.MaxActive, // no separate concurrency cap; alias of MaxActive
		"templates":      s.templates,
	}
}

func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.statsSnapshot())
}

func (s *Server) handleSandboxes(w http.ResponseWriter, _ *http.Request) {
	boxes := s.d.List("")
	out := make([]map[string]any, 0, len(boxes))
	for _, b := range boxes {
		out = append(out, map[string]any{
			"id": b.ID, "template": b.Template, "status": string(b.State),
			"boot_type": "process", "boot_ms": 0,
			"created_at": b.CreatedAt, "expires_at": b.ExpiresAt,
			"public_port": b.PublicPort,
		})
	}
	writeJSON(w, out)
}

func (s *Server) handleDestroy(w http.ResponseWriter, r *http.Request) {
	_ = s.d.Destroy(r.Context(), r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"max_active": s.limits.MaxActive, "max_concurrent": s.limits.MaxActive,
		"rate_limit_rps": s.limits.RateRPS, "rate_limit_burst": s.limits.RateBurst,
		"templates": s.templates, "team_name": "self-hosted", "team_id": "default",
	})
}

func (s *Server) handlePutConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{"status": "applied"})
}
