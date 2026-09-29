package orchestrator

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Launch stats feed the "about N seconds" estimate on the launch screen: the
// median of recent completed launches for a template, across the whole fleet.

const (
	// Below this the count is reported but the median withheld.
	minLaunchSamples  = 5
	launchStatsWindow = 30 * 24 * time.Hour
	maxStatTemplates  = 16
)

type launchStat struct {
	// P50Ms is 0 when there is not enough data; check N.
	P50Ms int64 `json:"p50_ms"`
	N     int   `json:"n"`
}

// registerLaunchStatsRoutes mounts /v1/launch-stats. No store check here: the
// store opens in a background goroutine and is nil when Routes() runs, so the
// nil case is handled per request by projectStore.
func (s *Server) registerLaunchStatsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/launch-stats", s.auth(s.handleLaunchStatsGet))
	mux.HandleFunc("POST /v1/launch-stats", s.auth(s.handleLaunchStatsPost))
}

func (s *Server) handleLaunchStatsGet(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	names := splitTemplates(r.URL.Query().Get("templates"))
	if len(names) == 0 {
		writeErr(w, http.StatusBadRequest, `"templates" is required`)
		return
	}
	since := time.Now().Add(-launchStatsWindow).Unix()
	out := map[string]launchStat{}
	for _, name := range names {
		p50, n := st.LaunchP50(r.Context(), name, since)
		// Every template asked about appears, so "never seen" is distinct from a typo.
		if n < minLaunchSamples {
			p50 = 0
		}
		out[name] = launchStat{P50Ms: p50, N: n}
	}
	writeJSON(w, http.StatusOK, map[string]any{"stats": out})
}

func (s *Server) handleLaunchStatsPost(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	var body struct {
		Template string `json:"template"`
		Ms       int64  `json:"ms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	name := strings.TrimSpace(body.Template)
	if name == "" {
		writeErr(w, http.StatusBadRequest, `"template" is required`)
		return
	}
	// The authenticated org is recorded so a poisoned median is traceable.
	if err := st.RecordLaunchSample(r.Context(), fastOrg(r), name, body.Ms, time.Now().Unix()); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not record")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func splitTemplates(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
		if len(out) == maxStatTemplates {
			break
		}
	}
	return out
}
