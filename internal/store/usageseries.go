package store

import (
	"context"
	"sync"
	"time"
)

// UsagePoint is one second of the usage chart (concurrency + start rate).
type UsagePoint struct {
	Time       int64 `json:"time"`
	Concurrent int   `json:"concurrent"`
	StartRate  int   `json:"start_rate"`
}

// UsageSeries returns the last 60s of concurrency and start rate for an org,
// plus the current running count, derived from running sandboxes and history spans.
func (s *Store) UsageSeries(ctx context.Context, orgID string) ([]UsagePoint, int) {
	now := time.Now().Unix()
	start := now - 59
	type span struct{ from, to int64 } // to==0 → still running
	var running []SandboxRecord
	var hist []Span
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); running, _ = s.SandboxesByOrg(ctx, orgID) }()
	go func() { defer wg.Done(); hist, _ = s.HistorySpans(ctx, orgID, start) }()
	wg.Wait()
	spans := make([]span, 0, len(running)+len(hist))
	for _, r := range running {
		spans = append(spans, span{r.CreatedAt, 0})
	}
	for _, h := range hist {
		spans = append(spans, span{h.CreatedAt, h.DestroyedAt})
	}
	pts := make([]UsagePoint, 0, 60)
	for t := start; t <= now; t++ {
		var conc, starts int
		for _, sp := range spans {
			if sp.from <= t && (sp.to == 0 || sp.to > t) {
				conc++
			}
			if sp.from == t {
				starts++
			}
		}
		pts = append(pts, UsagePoint{Time: t, Concurrent: conc, StartRate: starts})
	}
	return pts, len(running)
}
