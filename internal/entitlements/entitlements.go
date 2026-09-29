// Package entitlements defines the per-tenant resource caps (vCPU, memory,
// storage, concurrency, session TTL, persistence) that admission control and
// the scheduler enforce. There is no billing here.
package entitlements

// Plan is a named set of caps. An org with no assigned plan is unlimited.
type Plan struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	MaxConcurrent int    `json:"max_concurrent"` // 0 = unlimited
	SessionTTLHrs int    `json:"session_ttl_hrs"`
	MaxVCPU       int    `json:"max_vcpu"`
	MaxMemMB      int    `json:"max_mem_mb"`
	MaxStorageMB  int    `json:"max_storage_mb"`
	// Persistent lets sandboxes opt out of the hard TTL (create with timeout < 0).
	Persistent bool `json:"persistent"`
	// IncludedHours is the monthly allowance of active (running, not paused) hours.
	// 0 means unmetered. Measured and displayed, not enforced.
	IncludedHours int `json:"included_hours"`
	// MaxWorkstations caps persistent sandboxes only; MaxConcurrent counts all sandboxes.
	MaxWorkstations int `json:"max_workstations"`
	// MaxForwards caps the distinct guest ports one sandbox may publish on a
	// public TCP/UDP port. 0 means the default (DefaultMaxForwards); a plan
	// that should allow none says so with a negative number.
	MaxForwards int `json:"max_forwards"`
}

// DefaultMaxForwards is the per-sandbox port-forward cap a plan that names none gets.
const DefaultMaxForwards = 3

// Forwards resolves this plan's per-sandbox forward cap: 0 means the default,
// negative means none allowed.
func (p Plan) Forwards() int {
	switch {
	case p.MaxForwards == 0:
		return DefaultMaxForwards
	case p.MaxForwards < 0:
		return 0
	default:
		return p.MaxForwards
	}
}

// HoursUsage is a plan's active-hour entitlement measured against actual use.
type HoursUsage struct {
	Used     float64 `json:"used"`
	Included int     `json:"included"`
	// Remaining floors at zero.
	Remaining float64 `json:"remaining"`
	// Pct is percent of the allowance consumed, 0 when unmetered.
	Pct float64 `json:"pct"`
}

// Hours measures used active-hours against this plan's allowance.
func (p Plan) Hours(used float64) HoursUsage {
	h := HoursUsage{Used: used, Included: p.IncludedHours}
	if p.IncludedHours <= 0 {
		return h
	}
	h.Remaining = float64(p.IncludedHours) - used
	if h.Remaining < 0 {
		h.Remaining = 0
	}
	h.Pct = used / float64(p.IncludedHours) * 100
	return h
}

// DefaultPlanID is the plan a brand-new org starts on.
const DefaultPlanID = "free"

// Plans returns the catalogue loaded via SetCatalog, else the compiled presets.
func Plans() []Plan {
	if c := catalog.Load(); c != nil && len(*c) > 0 {
		return *c
	}
	return defaultPlans()
}

// defaultPlans must stay monotonic: each preset at least as generous as the
// previous one on every axis.
func defaultPlans() []Plan {
	return []Plan{
		{ID: "free", Name: "Small", MaxConcurrent: 5, SessionTTLHrs: 4, MaxVCPU: 2, MaxMemMB: 2048, MaxStorageMB: 2048, IncludedHours: 10, MaxWorkstations: 0},

		{ID: "personal", Name: "Personal", MaxConcurrent: 6, SessionTTLHrs: 24, MaxVCPU: 2, MaxMemMB: 4096, MaxStorageMB: 10240, Persistent: true, IncludedHours: 30, MaxWorkstations: 1},

		{ID: "pro", Name: "Standard", MaxConcurrent: 10, SessionTTLHrs: 24, MaxVCPU: 4, MaxMemMB: 8192, MaxStorageMB: 25600, Persistent: true, IncludedHours: 100, MaxWorkstations: 1},

		{ID: "max", Name: "Large", MaxConcurrent: 25, SessionTTLHrs: 24, MaxVCPU: 8, MaxMemMB: 16384, MaxStorageMB: 61440, Persistent: true, IncludedHours: 300, MaxWorkstations: 3, MaxForwards: 10},

		// Still a cap; an org with no plan at all is unlimited.
		{ID: "enterprise", Name: "Unrestricted", MaxConcurrent: 1000, SessionTTLHrs: 24, MaxVCPU: 8, MaxMemMB: 16384, MaxStorageMB: 102400, Persistent: true, MaxWorkstations: 1000, MaxForwards: 100},
	}
}

// PlanByID looks up a plan by id. Returns the free plan and false for an unknown id.
func PlanByID(id string) (Plan, bool) {
	for _, p := range Plans() {
		if p.ID == id {
			return p, true
		}
	}
	return Plans()[0], false
}

// DefaultPlan returns the free plan.
func DefaultPlan() Plan { return Plans()[0] }

// Unlimited is the plan an org with no assigned preset gets: no concurrency or
// session cap and generous per-box ceilings. This is the self-hosting default.
func Unlimited() Plan {
	return Plan{
		ID: "unlimited", Name: "Unlimited",
		MaxConcurrent: 0, SessionTTLHrs: 0,
		MaxVCPU: 16, MaxMemMB: 32768, MaxStorageMB: 102400,
		Persistent: true, MaxForwards: 100,
	}
}
