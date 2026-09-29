package httpapi

import (
	"net/http"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// handleEventsList returns the org's recent lifecycle events, newest first. GET /v1/events?limit=N
func (s *Server) handleEventsList(w http.ResponseWriter, r *http.Request) {
	limit := httpx.QueryInt(r, "limit", 50)
	recs, err := s.st.ListEvents(r.Context(), orgID(r), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if recs == nil {
		recs = []store.EventRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": recs})
}

// rangeSeconds maps the chart's range token to a lookback window.
func rangeSeconds(tok string) int64 {
	switch tok {
	case "5m":
		return 5 * 60
	case "1h":
		return 60 * 60
	case "1d":
		return 24 * 60 * 60
	default:
		return 60 * 60
	}
}

// concPoint is one (unix time, running count) sample on the concurrency chart.
type concPoint struct {
	At int64 `json:"at"`
	N  int   `json:"n"`
}

// handleConcurrency returns the org's running-sandbox count over the window as
// a stepwise series ending at now. GET /v1/concurrency?range=5m|1h|1d
func (s *Server) handleConcurrency(w http.ResponseWriter, r *http.Request) {
	rng := r.URL.Query().Get("range")
	now := time.Now().Unix()
	since := now - rangeSeconds(rng)
	recs, err := s.st.ConcurrencySeries(r.Context(), orgID(r), since)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	points := make([]concPoint, 0, len(recs)+2)
	for _, rec := range recs {
		points = append(points, concPoint{At: rec.At, N: rec.ActiveAfter})
	}
	cur := runningNow(s.d, orgID(r))
	if len(points) == 0 {
		points = append(points, concPoint{At: since, N: cur})
	}
	points = append(points, concPoint{At: now, N: cur})
	writeJSON(w, http.StatusOK, map[string]any{"range": rng, "points": points})
}

// runningNow counts an org's currently-running sandboxes on this node.
func runningNow(d *daemon.Daemon, org string) int {
	n := 0
	for _, sb := range d.List(org) {
		if sb.State == daemon.StateRunning {
			n++
		}
	}
	return n
}
