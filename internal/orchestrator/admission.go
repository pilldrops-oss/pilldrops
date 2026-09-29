package orchestrator

// Admission decides whether an org may start another sandbox. It lives here
// rather than on the node because only the store sees the whole fleet.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/entitlements"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// admissionUnavailable is the multi-tenant fail-closed message. It is the same
// for every cause so a tenant cannot probe which query failed.
const admissionUnavailable = "cannot verify your plan's limits right now, so new sandboxes are temporarily refused — retry shortly"

// multiTenantFromEnv reads SANDBOXD_MODE. Self-hosted (the default) fails open
// on store errors; multi-tenant fails closed because limits are a security boundary.
func multiTenantFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SANDBOXD_MODE"))) {
	case "multi-tenant", "multitenant", "multi_tenant":
		return true
	default:
		return false
	}
}

// admitBody claims a concurrency slot for the calling org. The caller must
// invoke release once the create has finished either way. body decides which
// caps apply (a negative timeout means a workstation).
func (s *Server) admitBody(r *http.Request, body []byte) (release func(), msg string, ok bool) {
	release, msg, ok = s.admitDecide(r, body)
	// Classify by the sentinel, not the message text, so rewording cannot relabel the metric.
	switch {
	case ok:
		s.metrics.recordAdmission(admitAllowed)
	case msg == admissionUnavailable:
		s.metrics.recordAdmission(admitUnavailable)
	default:
		s.metrics.recordAdmission(admitDenied)
	}
	return release, msg, ok
}

// admitDecide is admitBody's decision, split out so the outcome is counted in one place.
func (s *Server) admitDecide(r *http.Request, body []byte) (release func(), msg string, ok bool) {
	noop := func() {}
	st := s.st.Load()
	if st == nil {
		// Multi-tenant must not serve with admission silently disabled.
		if s.multiTenant {
			return noop, admissionUnavailable, false
		}
		return noop, "", true
	}
	org := fastOrg(r)
	if org == "" || org == "default" {
		return noop, "", true // operator/unscoped calls are not a customer's quota
	}
	// Multi-tenant refuses on both a read error and a missing row: an attacker
	// who can degrade the database must not turn every cap into unlimited, and
	// a new org must not be uncapped until an operator runs setplan.
	a, found, err := st.PlanFor(r.Context(), org)
	switch {
	case err != nil:
		if s.multiTenant {
			s.log().ErrorContext(r.Context(), "plan lookup failed, refusing (multi-tenant)",
				"component", "admission", "org", org, "err", err)
			return noop, admissionUnavailable, false
		}
		return noop, "", true
	case !found:
		if s.multiTenant {
			return noop, "no plan is assigned to this organisation, so no quota can be enforced — an operator must assign one before sandboxes can be created", false
		}
		return noop, "", true
	}
	plan, ok := entitlements.PlanByID(a.PlanID)
	if !ok {
		if s.multiTenant {
			s.log().ErrorContext(r.Context(), "assigned plan id is not in the catalogue, refusing (multi-tenant)",
				"component", "admission", "org", org, "plan", a.PlanID)
			return noop, admissionUnavailable, false
		}
		return noop, "", true
	}
	// Checked before the slot is reserved so a refusal leaks no claim. A plain
	// count, not an atomic claim: a burst can slip past, but it is bounded by
	// the concurrency slot below.
	if plan.Persistent && plan.MaxWorkstations > 0 && wantsWorkstation(body) {
		n, cErr := st.CountPersistentForOrg(r.Context(), org)
		switch {
		case cErr != nil:
			if s.multiTenant {
				s.log().ErrorContext(r.Context(), "persistent workstation count unreadable, refusing (multi-tenant)",
					"component", "admission", "org", org, "err", cErr)
				return noop, admissionUnavailable, false
			}
			s.log().WarnContext(r.Context(), "persistent workstation count unreadable, allowing",
				"component", "admission", "org", org, "err", cErr)
		case n >= plan.MaxWorkstations:
			return noop, fmt.Sprintf(
				"plan '%s' includes %d persistent workstation(s) and you already have %d — delete one or upgrade",
				plan.ID, plan.MaxWorkstations, n), false
		}
	}

	if plan.MaxConcurrent <= 0 {
		return noop, "", true // unlimited
	}

	slot := "slot_" + org + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if s.admit != nil {
		if release, ok := s.admitRedis(r, st, org, slot, plan.MaxConcurrent); ok {
			if release == nil {
				return noop, fmt.Sprintf(
					"plan '%s' allows %d concurrent sandboxes and you are already at the limit — pause or destroy one, or upgrade",
					plan.ID, plan.MaxConcurrent), false
			}
			return release, "", true
		}
		// Redis or the live count failed: the Postgres path below decides.
	}
	admitted, err := st.ReserveSlot(r.Context(), org, slot, plan.MaxConcurrent)
	if err != nil {
		if s.multiTenant {
			s.log().ErrorContext(r.Context(), "concurrency slot reservation failed, refusing (multi-tenant)",
				"component", "admission", "org", org, "err", err)
			return noop, admissionUnavailable, false
		}
		s.log().WarnContext(r.Context(), "concurrency slot reservation failed, allowing",
			"component", "admission", "org", org, "err", err)
		return noop, "", true
	}
	if !admitted {
		return noop, fmt.Sprintf(
			"plan '%s' allows %d concurrent sandboxes and you are already at the limit — pause or destroy one, or upgrade",
			plan.ID, plan.MaxConcurrent), false
	}
	return func() { _ = st.ReleaseSlot(r.Context(), slot) }, "", true
}

// wantsWorkstation reports whether a create body asks for a persistent
// workstation (negative timeout, the same rule the node applies). An empty or
// unparseable body is not a workstation request; the node still checks its own decode.
func wantsWorkstation(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var req struct {
		Timeout int `json:"timeout"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}
	return req.Timeout < 0
}

// admitRedis runs the concurrency check in Redis. ok=false means it could not
// decide and the caller falls back to Postgres; release=nil with ok=true is a
// refusal.
func (s *Server) admitRedis(r *http.Request, st *store.Store, org, slot string, cap int) (release func(), ok bool) {
	live, err := st.CountLiveForOrg(r.Context(), org)
	if err != nil {
		return nil, false
	}
	admitted, err := s.admit.Reserve(r.Context(), org, slot, live, cap, time.Now())
	if err != nil {
		s.log().WarnContext(r.Context(), "redis admission unavailable; using Postgres",
			"component", "admission", "org", org, "err", err)
		return nil, false
	}
	if !admitted {
		return nil, true
	}
	return func() { _ = s.admit.Release(context.Background(), org, slot, time.Now()) }, true
}
