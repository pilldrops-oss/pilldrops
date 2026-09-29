package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/entitlements"
)

// Entitlements meter usage and enforce plan caps. A plan is only a named set
// of caps; nothing here charges money.

// cycleDays is the rolling window active hours are measured over.
const cycleDays = 30

// orgPlan returns the caps an org is subject to. An org with no plan assigned
// is unlimited, so a self-hosted deployment is never capped by accident.
func (s *Server) orgPlan(ctx context.Context, org string) entitlements.Plan {
	a, ok, err := s.st.PlanFor(ctx, org)
	if err != nil || !ok || a.PlanID == "" {
		return entitlements.Unlimited()
	}
	plan, ok := entitlements.PlanByID(a.PlanID)
	if !ok {
		return entitlements.Unlimited()
	}
	return plan
}

// handleEntitlements reports what this org may use and what it has used.
//
//	GET /v1/limits
func (s *Server) handleEntitlements(w http.ResponseWriter, r *http.Request) {
	org := orgID(r)
	plan := s.orgPlan(r.Context(), org)
	writeJSON(w, http.StatusOK, map[string]any{
		"plan": plan,
		// Hours are measured and shown, never enforced.
		"hours":   plan.Hours(s.activeHoursThisCycle(r.Context(), org, 0)),
		"storage": s.orgStorage(r.Context(), org, plan.MaxStorageMB),
		"plans":   entitlements.Plans(),
	})
}

// activeHoursThisCycle sums running (not paused) hours over the trailing cycle.
func (s *Server) activeHoursThisCycle(ctx context.Context, org string, since int64) float64 {
	if since <= 0 {
		since = time.Now().Unix() - int64(cycleDays*24*60*60)
	}
	h, err := s.st.ActiveHoursSince(ctx, org, since)
	if err != nil {
		return 0
	}
	return h
}

// orgStorage sums the cached disk usage of the org's live sandboxes. capMB is
// the plan's per-sandbox ceiling, not a total.
func (s *Server) orgStorage(ctx context.Context, org string, capMB int) map[string]any {
	usedMB, measured, n := 0, false, 0
	for _, sb := range s.d.List(org) {
		n++
		if used, ok := s.d.StorageUsedMB(ctx, sb.ID); ok {
			usedMB += used
			measured = true
		}
	}
	return map[string]any{
		"used_mb":   usedMB,
		"cap_mb":    capMB,
		"sandboxes": n,
		"measured":  measured, // false means the runtime cannot report, so the UI shows n/a
	}
}
