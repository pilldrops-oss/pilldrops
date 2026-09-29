package orchestrator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// statsStreamPeriod matches the node's stream; the payload is in-memory registry state.
const statsStreamPeriod = time.Second

// statsStreamMax bounds one connection so http.Server.Shutdown can drain;
// EventSource reconnects on its own.
const statsStreamMax = 30 * time.Minute

// fleetLoad totals live sandboxes across every node and standard-sandbox
// headroom across schedulable ones.
func (s *Server) fleetLoad(now time.Time) (active, headroom int) {
	for _, n := range s.reg.Nodes() {
		active += n.Cap.Active
		if n.Schedulable(now, cluster.NodeTTL) {
			headroom += n.StandardFits(now, cluster.NodeTTL)
		}
	}
	return active, headroom
}

// fleetStats matches the node's stats event field-for-field so the same page consumes either.
func (s *Server) fleetStats() map[string]any {
	active, headroom := s.fleetLoad(s.now())
	return map[string]any{
		"active_count": active,
		"max_active":   active + headroom,
		// Present and empty rather than absent: the client types this as a record.
		"pool_stats": map[string]any{},
	}
}

// handleDashboardStats serves one fleet-stats snapshot, the polling twin of the stream.
func (s *Server) handleDashboardStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, s.fleetStats())
}

// handleDashboardEvents streams fleet stats as Server-Sent Events.
func (s *Server) handleDashboardEvents(w http.ResponseWriter, r *http.Request) {
	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	send := func() {
		data, err := json.Marshal(s.fleetStats())
		if err != nil {
			return // skip the tick, keep the stream
		}
		fmt.Fprintf(w, "event: stats\ndata: %s\n\n", data)
		f.Flush()
	}
	send()

	ticker := time.NewTicker(statsStreamPeriod)
	defer ticker.Stop()
	deadline := time.NewTimer(statsStreamMax)
	defer deadline.Stop()

	for {
		select {
		case <-ticker.C:
			send()
		case <-deadline.C:
			return // client reconnects
		case <-r.Context().Done():
			return
		}
	}
}
